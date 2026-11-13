package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"net/url"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
)

// The service is its own OAuth 2.1 authorization server for one public
// client, the browser extension: authorization code + PKCE, short-lived
// access tokens and single-use rotating refresh tokens.

const (
	oauthClientID = "invidioslop"
	codeTTL       = 2 * time.Minute
	accessTTL     = time.Hour
	refreshTTL    = 90 * 24 * time.Hour
)

type authRequest struct {
	RedirectURI string `json:"redirect_uri"`
	Challenge   string `json:"challenge"`
	State       string `json:"-"`
}

// parseAuthRequest validates the parameters that must be right before we
// are willing to redirect anywhere.
func (s *Server) parseAuthRequest(q url.Values) (*authRequest, error) {
	if q.Get("client_id") != oauthClientID {
		return nil, errf(http.StatusBadRequest, "invalid_client", "unknown client")
	}
	ar := &authRequest{RedirectURI: q.Get("redirect_uri"), Challenge: q.Get("code_challenge"), State: q.Get("state")}
	if !slices.Contains(s.cfg.RedirectURIs, ar.RedirectURI) {
		return nil, errf(http.StatusBadRequest, "invalid_request", "redirect_uri is not registered")
	}
	if q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" || len(ar.Challenge) < 43 || len(ar.Challenge) > 128 {
		return nil, errf(http.StatusBadRequest, "invalid_request", "authorization code with S256 PKCE is required")
	}
	if len(ar.State) > 512 {
		return nil, errf(http.StatusBadRequest, "invalid_request", "state too long")
	}
	return ar, nil
}

func redirectWith(w http.ResponseWriter, r *http.Request, base string, params url.Values) {
	u, _ := url.Parse(base)
	q := u.Query()
	for k, v := range params {
		q[k] = v
	}
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusSeeOther)
}

// handleAuthorize shows the consent screen (after sign-in if needed).
func (s *Server) handleAuthorize(w http.ResponseWriter, r *http.Request) error {
	ar, err := s.parseAuthRequest(r.URL.Query())
	if err != nil {
		return s.page(w, r, http.StatusBadRequest, "error", map[string]any{"Message": err.(*apiError).Msg})
	}
	u := s.sessionUser(r)
	if u == nil {
		http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
		return nil
	}
	return s.page(w, r, http.StatusOK, "consent", map[string]any{"User": u, "Query": r.URL.Query(), "Req": ar})
}

// handleAuthorizeDecision issues the code once the user approves.
func (s *Server) handleAuthorizeDecision(w http.ResponseWriter, r *http.Request) error {
	if err := s.sameOrigin(r); err != nil {
		return err
	}
	if err := r.ParseForm(); err != nil {
		return errf(http.StatusBadRequest, "bad_request", "invalid form")
	}
	ar, err := s.parseAuthRequest(r.PostForm)
	if err != nil {
		return err
	}
	u := s.sessionUser(r)
	if u == nil {
		return errf(http.StatusUnauthorized, "unauthorized", "sign in first")
	}
	out := url.Values{}
	if ar.State != "" {
		out.Set("state", ar.State)
	}
	if r.PostForm.Get("decision") != "allow" {
		out.Set("error", "access_denied")
		redirectWith(w, r, ar.RedirectURI, out)
		return nil
	}
	code := randToken(32)
	if err := s.putEphemeral(r.Context(), code, "oauth-code", &u.ID, ar, codeTTL); err != nil {
		return err
	}
	out.Set("code", code)
	redirectWith(w, r, ar.RedirectURI, out)
	return nil
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
}

func (s *Server) issueTokens(ctx context.Context, tx pgx.Tx, userID, family string) (*tokenResponse, error) {
	now := s.now()
	access, refresh := randToken(32), randToken(32)
	if _, err := tx.Exec(ctx,
		`INSERT INTO tokens (hash, user_id, kind, family, expires_at) VALUES ($1, $3, 'access', $4, $5), ($2, $3, 'refresh', $4, $6)`,
		hashHex(access), hashHex(refresh), userID, family, now.Add(accessTTL), now.Add(refreshTTL)); err != nil {
		return nil, err
	}
	return &tokenResponse{AccessToken: access, TokenType: "Bearer", ExpiresIn: int(accessTTL / time.Second), RefreshToken: refresh}, nil
}

func oauthErr(code, msg string) *apiError {
	return &apiError{Status: http.StatusBadRequest, Code: code, Msg: msg}
}

func (s *Server) handleToken(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	if err := s.rateLimit(ctx, r, "token", 1, 600, time.Hour); err != nil {
		return err
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := r.ParseForm(); err != nil {
		return oauthErr("invalid_request", "form-encoded body required")
	}
	f := r.PostForm
	if f.Get("client_id") != oauthClientID {
		return oauthErr("invalid_client", "unknown client")
	}

	var resp *tokenResponse
	switch f.Get("grant_type") {
	case "authorization_code":
		var ar authRequest
		uid, err := s.takeEphemeral(ctx, f.Get("code"), "oauth-code", &ar)
		if err != nil || uid == nil {
			return oauthErr("invalid_grant", "code is invalid or expired")
		}
		sum := sha256.Sum256([]byte(f.Get("code_verifier")))
		want := base64.RawURLEncoding.EncodeToString(sum[:])
		if subtle.ConstantTimeCompare([]byte(want), []byte(ar.Challenge)) != 1 || f.Get("redirect_uri") != ar.RedirectURI {
			return oauthErr("invalid_grant", "PKCE verification failed")
		}
		err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
			var family string
			if err := tx.QueryRow(ctx, `SELECT gen_random_uuid()::text`).Scan(&family); err != nil {
				return err
			}
			resp, err = s.issueTokens(ctx, tx, *uid, family)
			return err
		})
		if err != nil {
			return err
		}

	case "refresh_token":
		var invalid bool
		err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
			var uid, family string
			var used bool
			err := tx.QueryRow(ctx,
				`SELECT user_id::text, family::text, used FROM tokens
				  WHERE hash = $1 AND kind = 'refresh' AND expires_at > $2 FOR UPDATE`,
				hashHex(f.Get("refresh_token")), s.now()).Scan(&uid, &family, &used)
			if err != nil {
				invalid = true
				return nil
			}
			if used {
				// A refresh token was replayed: assume theft, end the sign-in.
				invalid = true
				_, err := tx.Exec(ctx, `DELETE FROM tokens WHERE family = $1`, family)
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE tokens SET used = true WHERE hash = $1`, hashHex(f.Get("refresh_token"))); err != nil {
				return err
			}
			resp, err = s.issueTokens(ctx, tx, uid, family)
			return err
		})
		if err != nil {
			return err
		}
		if invalid {
			return oauthErr("invalid_grant", "refresh token is invalid or expired")
		}

	default:
		return oauthErr("unsupported_grant_type", "use authorization_code or refresh_token")
	}
	writeJSON(w, http.StatusOK, resp)
	return nil
}

// handleRevoke signs the extension out everywhere that sign-in was used.
func (s *Server) handleRevoke(w http.ResponseWriter, r *http.Request) error {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := r.ParseForm(); err != nil {
		return oauthErr("invalid_request", "form-encoded body required")
	}
	_, err := s.db.Exec(r.Context(),
		`DELETE FROM tokens WHERE family IN (SELECT family FROM tokens WHERE hash = $1)`,
		hashHex(r.PostForm.Get("token")))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	return nil
}
