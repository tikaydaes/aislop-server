package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/jackc/pgx/v5"
)

// Accounts are passkey-only: no email, no password, no recovery. The
// account is a random id and a display name.

type User struct {
	ID         string
	Handle     []byte
	Name       string
	Reputation float64
	IsAdmin    bool
	Banned     bool
	creds      []webauthn.Credential
}

func (u *User) WebAuthnID() []byte                         { return u.Handle }
func (u *User) WebAuthnName() string                       { return u.Name }
func (u *User) WebAuthnDisplayName() string                { return u.Name }
func (u *User) WebAuthnCredentials() []webauthn.Credential { return u.creds }

const (
	sessionCookie = "sid"
	sessionTTL    = 30 * 24 * time.Hour
	ceremonyTTL   = 5 * time.Minute
	powTTL        = 10 * time.Minute
	signupsPerDay = 3 // per network
)

const userCols = `id::text, wa_handle, name, reputation, is_admin, banned`

func scanUser(row pgx.Row) (*User, error) {
	u := &User{}
	if err := row.Scan(&u.ID, &u.Handle, &u.Name, &u.Reputation, &u.IsAdmin, &u.Banned); err != nil {
		return nil, err
	}
	return u, nil
}

func (s *Server) userByID(ctx context.Context, id string) (*User, error) {
	return scanUser(s.db.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE id = $1`, id))
}

func (s *Server) loadCreds(ctx context.Context, u *User) error {
	rows, err := s.db.Query(ctx, `SELECT data FROM credentials WHERE user_id = $1`, u.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	u.creds = nil
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return err
		}
		var c webauthn.Credential
		if err := json.Unmarshal(raw, &c); err != nil {
			return err
		}
		u.creds = append(u.creds, c)
	}
	return rows.Err()
}

// ------------------------------------------------------- ephemeral store

func (s *Server) putEphemeral(ctx context.Context, secret, kind string, userID *string, data any, ttl time.Duration) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(ctx,
		`INSERT INTO ephemeral (id, kind, user_id, data, expires_at) VALUES ($1, $2, $3, $4, $5)`,
		hashHex(secret), kind, userID, raw, s.now().Add(ttl))
	return err
}

// takeEphemeral fetches and deletes in one step, so every secret is single-use.
func (s *Server) takeEphemeral(ctx context.Context, secret, kind string, data any) (userID *string, err error) {
	var raw []byte
	err = s.db.QueryRow(ctx,
		`DELETE FROM ephemeral WHERE id = $1 AND kind = $2 AND expires_at > $3 RETURNING user_id::text, data`,
		hashHex(secret), kind, s.now()).Scan(&userID, &raw)
	if err != nil {
		return nil, err
	}
	if data != nil {
		err = json.Unmarshal(raw, data)
	}
	return userID, err
}

// ---------------------------------------------------------- web sessions

func (s *Server) startSession(ctx context.Context, w http.ResponseWriter, userID string) error {
	secret := randToken(32)
	if err := s.putEphemeral(ctx, secret, "session", &userID, struct{}{}, sessionTTL); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: secret, Path: "/", MaxAge: int(sessionTTL / time.Second),
		HttpOnly: true, Secure: strings.HasPrefix(s.cfg.Origin, "https://"), SameSite: http.SameSiteLaxMode,
	})
	return nil
}

func (s *Server) endSession(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		_, _ = s.db.Exec(ctx, `DELETE FROM ephemeral WHERE id = $1 AND kind = 'session'`, hashHex(c.Value))
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1})
}

// sessionUser returns the signed-in web user, or nil.
func (s *Server) sessionUser(r *http.Request) *User {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return nil
	}
	u, err := scanUser(s.db.QueryRow(r.Context(),
		`SELECT `+prefixCols("u", userCols)+` FROM ephemeral e JOIN users u ON u.id = e.user_id
		  WHERE e.id = $1 AND e.kind = 'session' AND e.expires_at > $2`, hashHex(c.Value), s.now()))
	if err != nil {
		return nil
	}
	return u
}

func prefixCols(alias, cols string) string {
	parts := strings.Split(cols, ", ")
	for i, p := range parts {
		parts[i] = alias + "." + p
	}
	return strings.Join(parts, ", ")
}

// sameOrigin rejects cross-site state-changing requests made with the
// session cookie.
func (s *Server) sameOrigin(r *http.Request) error {
	if o := r.Header.Get("Origin"); o != s.cfg.Origin {
		return errf(http.StatusForbidden, "bad_origin", "request must come from %s", s.cfg.Origin)
	}
	return nil
}

// --------------------------------------------------------- proof of work

type powChallenge struct {
	Challenge string `json:"challenge"`
	Bits      int    `json:"bits"`
}

func (s *Server) handlePow(w http.ResponseWriter, r *http.Request) error {
	if err := s.rateLimit(r.Context(), r, "pow", 1, 60, time.Hour); err != nil {
		return err
	}
	ch := randToken(18)
	if err := s.putEphemeral(r.Context(), ch, "pow", nil, struct{}{}, powTTL); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, powChallenge{Challenge: ch, Bits: s.cfg.PowBits})
	return nil
}

// checkPow verifies sha256(challenge + ":" + nonce) has the required number
// of leading zero bits and spends the challenge.
func (s *Server) checkPow(ctx context.Context, challenge, nonce string) error {
	bad := errf(http.StatusBadRequest, "bad_pow", "proof of work missing or invalid; reload and try again")
	if challenge == "" || len(nonce) > 32 {
		return bad
	}
	sum := sha256.Sum256([]byte(challenge + ":" + nonce))
	if leadingZeroBits(sum[:]) < s.cfg.PowBits {
		return bad
	}
	if _, err := s.takeEphemeral(ctx, challenge, "pow", nil); err != nil {
		return bad
	}
	return nil
}

func leadingZeroBits(b []byte) int {
	n := 0
	for _, x := range b {
		if x == 0 {
			n += 8
			continue
		}
		for m := byte(0x80); m != 0 && x&m == 0; m >>= 1 {
			n++
		}
		break
	}
	return n
}

// -------------------------------------------------------------- passkeys

var nameAdjectives = []string{"Quiet", "Brisk", "Amber", "Lucid", "Mellow", "Nimble", "Plain", "Rapid", "Solid", "Tidal", "Vivid", "Wry", "Bold", "Calm", "Deft", "Keen"}
var nameNouns = []string{"Otter", "Heron", "Lynx", "Finch", "Marten", "Wren", "Ibis", "Tapir", "Gecko", "Kestrel", "Badger", "Dingo", "Quokka", "Numbat", "Bilby", "Emu"}

func randomName() string {
	pick := func(list []string) string {
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(list))))
		return list[n.Int64()]
	}
	var b [2]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%s%s%04d", pick(nameAdjectives), pick(nameNouns), binary.BigEndian.Uint16(b[:])%10000)
}

func cleanName(n string) string {
	n = strings.Join(strings.Fields(n), " ")
	if len([]rune(n)) > 32 {
		n = string([]rune(n)[:32])
	}
	return n
}

type ceremony struct {
	Session webauthn.SessionData `json:"session"`
	Name    string               `json:"name,omitempty"`
	Handle  []byte               `json:"handle,omitempty"`
}

var passkeySelection = protocol.AuthenticatorSelection{
	ResidentKey:      protocol.ResidentKeyRequirementRequired,
	UserVerification: protocol.VerificationPreferred,
}

// handleRegisterBegin starts creating an account (or, when signed in,
// adding another passkey to the current one).
func (s *Server) handleRegisterBegin(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	if err := s.sameOrigin(r); err != nil {
		return err
	}
	var req struct {
		Name      string `json:"name"`
		Challenge string `json:"challenge"`
		Nonce     string `json:"nonce"`
	}
	if err := readJSON(w, r, &req); err != nil {
		return err
	}

	var u *User
	c := ceremony{}
	if cur := s.sessionUser(r); cur != nil {
		if err := s.loadCreds(ctx, cur); err != nil {
			return err
		}
		if len(cur.creds) >= 5 {
			return errf(http.StatusBadRequest, "too_many", "an account can hold at most 5 passkeys")
		}
		u = cur
	} else {
		if err := s.rateLimit(ctx, r, "signup", 1, 10, time.Hour); err != nil {
			return err
		}
		if err := s.checkPow(ctx, req.Challenge, req.Nonce); err != nil {
			return err
		}
		var recent int
		if err := s.db.QueryRow(ctx,
			`SELECT count(*) FROM users WHERE net_key = $1 AND created_at > $2`,
			s.netKey(ctx, r), s.now().Add(-24*time.Hour)).Scan(&recent); err != nil {
			return err
		}
		if recent >= signupsPerDay {
			return errf(http.StatusTooManyRequests, "signup_limit", "too many new accounts from this network today")
		}
		name := cleanName(req.Name)
		if name == "" {
			name = randomName()
		}
		handle := make([]byte, 32)
		_, _ = rand.Read(handle)
		u = &User{Handle: handle, Name: name}
		c.Name, c.Handle = name, handle
	}

	var exclude []protocol.CredentialDescriptor
	for _, cr := range u.creds {
		exclude = append(exclude, cr.Descriptor())
	}
	creation, session, err := s.wa.BeginRegistration(u,
		webauthn.WithAuthenticatorSelection(passkeySelection),
		webauthn.WithExclusions(exclude))
	if err != nil {
		return err
	}
	c.Session = *session
	id := randToken(24)
	var uid *string
	if u.ID != "" {
		uid = &u.ID
	}
	if err := s.putEphemeral(ctx, id, "wa-register", uid, c, ceremonyTTL); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"ceremony": id, "options": creation})
	return nil
}

func (s *Server) handleRegisterFinish(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	if err := s.sameOrigin(r); err != nil {
		return err
	}
	var c ceremony
	uid, err := s.takeEphemeral(ctx, r.URL.Query().Get("ceremony"), "wa-register", &c)
	if err != nil {
		return errf(http.StatusBadRequest, "expired", "registration expired; start again")
	}
	var u *User
	if uid != nil {
		if u, err = s.userByID(ctx, *uid); err != nil {
			return err
		}
	} else {
		u = &User{Handle: c.Handle, Name: c.Name}
	}
	cred, err := s.wa.FinishRegistration(u, c.Session, r)
	if err != nil {
		return errf(http.StatusBadRequest, "passkey_failed", "the passkey could not be verified")
	}
	raw, err := json.Marshal(cred)
	if err != nil {
		return err
	}
	err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		if u.ID == "" {
			if err := tx.QueryRow(ctx,
				`INSERT INTO users (wa_handle, name, net_key, created_at) VALUES ($1, $2, $3, $4) RETURNING id::text`,
				u.Handle, u.Name, s.netKey(ctx, r), s.now()).Scan(&u.ID); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, `INSERT INTO credentials (id, user_id, data) VALUES ($1, $2, $3)`, cred.ID, u.ID, raw)
		return err
	})
	if err != nil {
		return err
	}
	if uid == nil {
		if err := s.startSession(ctx, w, u.ID); err != nil {
			return err
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "name": u.Name})
	return nil
}

func (s *Server) handleLoginBegin(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	if err := s.sameOrigin(r); err != nil {
		return err
	}
	if err := s.rateLimit(ctx, r, "login", 1, 120, time.Hour); err != nil {
		return err
	}
	assertion, session, err := s.wa.BeginDiscoverableLogin(webauthn.WithUserVerification(protocol.VerificationPreferred))
	if err != nil {
		return err
	}
	id := randToken(24)
	if err := s.putEphemeral(ctx, id, "wa-login", nil, ceremony{Session: *session}, ceremonyTTL); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"ceremony": id, "options": assertion})
	return nil
}

func (s *Server) handleLoginFinish(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	if err := s.sameOrigin(r); err != nil {
		return err
	}
	var c ceremony
	if _, err := s.takeEphemeral(ctx, r.URL.Query().Get("ceremony"), "wa-login", &c); err != nil {
		return errf(http.StatusBadRequest, "expired", "sign-in expired; start again")
	}
	var found *User
	lookup := func(rawID, userHandle []byte) (webauthn.User, error) {
		u, err := scanUser(s.db.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE wa_handle = $1`, userHandle))
		if err != nil {
			return nil, err
		}
		if err := s.loadCreds(ctx, u); err != nil {
			return nil, err
		}
		found = u
		return u, nil
	}
	cred, err := s.wa.FinishDiscoverableLogin(lookup, c.Session, r)
	if err != nil || found == nil {
		return errf(http.StatusUnauthorized, "passkey_failed", "that passkey is not registered here")
	}
	if raw, err := json.Marshal(cred); err == nil {
		// Persist the updated signature counter and backup flags.
		_, _ = s.db.Exec(ctx, `UPDATE credentials SET data = $2 WHERE id = $1`, cred.ID, raw)
	}
	if err := s.startSession(ctx, w, found.ID); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "name": found.Name})
	return nil
}

// ----------------------------------------------------------- API tokens

// bearerUser authenticates an API call made with an OAuth access token.
func (s *Server) bearerUser(r *http.Request) (*User, error) {
	unauth := errf(http.StatusUnauthorized, "unauthorized", "sign in to do this")
	h := r.Header.Get("Authorization")
	if len(h) < 8 || !strings.EqualFold(h[:7], "Bearer ") {
		return nil, unauth
	}
	u, err := scanUser(s.db.QueryRow(r.Context(),
		`SELECT `+prefixCols("u", userCols)+` FROM tokens t JOIN users u ON u.id = t.user_id
		  WHERE t.hash = $1 AND t.kind = 'access' AND t.expires_at > $2`, hashHex(h[7:]), s.now()))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, unauth
	}
	return u, err
}
