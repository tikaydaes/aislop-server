package server

import (
	"context"
	"crypto/subtle"
	"embed"
	"html/template"
	"io/fs"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

//go:embed web/*.html
var tmplFS embed.FS

//go:embed web/static/*
var staticFS embed.FS

var tmpl = template.Must(template.New("").Funcs(template.FuncMap{
	"rep":    func(f float64) string { return strconv.FormatFloat(math.Round(f*100)/100, 'f', -1, 64) },
	"weight": func(f float64) string { return strconv.FormatFloat(math.Round(voteWeight(f)*100)/100, 'f', -1, 64) },
	"types":  func(m int) string { return strings.Join(maskToTypes(m), ", ") },
	"date":   func(t time.Time) string { return t.UTC().Format("2006-01-02 15:04") },
	"status": func(i int) string { return statusNames[i] },
}).ParseFS(tmplFS, "web/*.html"))

// page renders one of the embedded templates inside the shared layout.
func (s *Server) page(w http.ResponseWriter, r *http.Request, status int, name string, data map[string]any) error {
	if data == nil {
		data = map[string]any{}
	}
	if _, ok := data["User"]; !ok {
		data["User"] = s.sessionUser(r)
	}
	data["Page"] = name
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:; base-uri 'none'; frame-ancestors 'none'")
	h.Set("X-Content-Type-Options", "nosniff")
	// same-origin, not no-referrer: the latter makes browsers send
	// "Origin: null" on form posts, which the same-origin check rejects.
	h.Set("Referrer-Policy", "same-origin")
	w.WriteHeader(status)
	return tmpl.ExecuteTemplate(w, name+".html", data)
}

// Routes returns the HTTP handler for the whole service.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	h := s.handler

	// Read side (anonymous).
	mux.HandleFunc("GET /v1/snapshot", h(s.handleSnapshot))
	mux.HandleFunc("GET /v1/delta", h(s.handleDelta))
	mux.HandleFunc("POST /v1/channels/lookup", h(s.handleLookup))
	mux.HandleFunc("POST /v1/channels/claims", h(s.handleClaims))
	mux.HandleFunc("GET /v1/videos/{id}", h(s.handleVideo))

	// Write side (signed in).
	mux.HandleFunc("POST /v1/assessments", h(s.handleAssess))
	mux.HandleFunc("GET /v1/me", h(s.handleMe))

	// Accounts and OAuth.
	mux.HandleFunc("GET /auth/pow", h(s.handlePow))
	mux.HandleFunc("POST /auth/register/begin", h(s.handleRegisterBegin))
	mux.HandleFunc("POST /auth/register/finish", h(s.handleRegisterFinish))
	mux.HandleFunc("POST /auth/login/begin", h(s.handleLoginBegin))
	mux.HandleFunc("POST /auth/login/finish", h(s.handleLoginFinish))
	mux.HandleFunc("GET /oauth/authorize", h(s.handleAuthorize))
	mux.HandleFunc("POST /oauth/authorize", h(s.handleAuthorizeDecision))
	mux.HandleFunc("POST /oauth/token", h(s.handleToken))
	mux.HandleFunc("POST /oauth/revoke", h(s.handleRevoke))

	// Pages.
	mux.HandleFunc("GET /{$}", h(func(w http.ResponseWriter, r *http.Request) error {
		return s.page(w, r, http.StatusOK, "home", nil)
	}))
	mux.HandleFunc("GET /login", h(s.handleLoginPage))
	mux.HandleFunc("POST /logout", h(s.handleLogout))
	mux.HandleFunc("GET /account", h(s.handleAccount))
	mux.HandleFunc("POST /account/name", h(s.handleAccountName))
	mux.HandleFunc("POST /account/delete", h(s.handleAccountDelete))
	mux.HandleFunc("GET /contest", h(func(w http.ResponseWriter, r *http.Request) error {
		return s.page(w, r, http.StatusOK, "contest", nil)
	}))
	mux.HandleFunc("POST /contest", h(s.handleContest))

	// Admin.
	mux.HandleFunc("GET /admin", h(s.handleAdmin))
	mux.HandleFunc("GET /admin/claim", h(s.handleAdminClaim))
	mux.HandleFunc("POST /admin/video", h(s.handleAdminVideo))
	mux.HandleFunc("POST /admin/channel", h(s.handleAdminChannel))
	mux.HandleFunc("POST /admin/user", h(s.handleAdminUser))
	mux.HandleFunc("POST /admin/contest", h(s.handleAdminContest))
	mux.HandleFunc("POST /admin/hold", h(s.handleAdminHold))

	static, _ := fs.Sub(staticFS, "web/static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	mux.HandleFunc("GET /healthz", h(func(w http.ResponseWriter, r *http.Request) error {
		if err := s.db.Ping(r.Context()); err != nil {
			return err
		}
		_, _ = w.Write([]byte("ok\n"))
		return nil
	}))
	return mux
}

// safeNext only allows redirects back into this site.
func safeNext(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.Contains(next, "\\") {
		return "/account"
	}
	return next
}

func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) error {
	next := safeNext(r.URL.Query().Get("next"))
	if s.sessionUser(r) != nil {
		http.Redirect(w, r, next, http.StatusSeeOther)
		return nil
	}
	return s.page(w, r, http.StatusOK, "login", map[string]any{"Next": next})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) error {
	if err := s.sameOrigin(r); err != nil {
		return err
	}
	s.endSession(r.Context(), w, r)
	http.Redirect(w, r, "/", http.StatusSeeOther)
	return nil
}

type repEvent struct {
	Video    *string
	Delta    float64
	Reason   string
	Reversed bool
	At       time.Time
}

func (s *Server) handleAccount(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	u := s.sessionUser(r)
	if u == nil {
		http.Redirect(w, r, "/login?next=/account", http.StatusSeeOther)
		return nil
	}
	var passkeys, reports int
	if err := s.db.QueryRow(ctx,
		`SELECT (SELECT count(*) FROM credentials WHERE user_id = $1), (SELECT count(*) FROM assessments WHERE user_id = $1)`,
		u.ID).Scan(&passkeys, &reports); err != nil {
		return err
	}
	rows, err := s.db.Query(ctx,
		`SELECT video_id, applied, reason, reversed, created_at FROM rep_events WHERE user_id = $1 AND (reversed OR applied <> 0) ORDER BY id DESC LIMIT 25`, u.ID)
	if err != nil {
		return err
	}
	events, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (repEvent, error) {
		var e repEvent
		err := row.Scan(&e.Video, &e.Delta, &e.Reason, &e.Reversed, &e.At)
		return e, err
	})
	if err != nil {
		return err
	}
	return s.page(w, r, http.StatusOK, "account", map[string]any{
		"User": u, "Passkeys": passkeys, "Reports": reports, "Events": events,
		"Limit": dailyReportLimit(u.Reputation),
	})
}

func (s *Server) handleAccountName(w http.ResponseWriter, r *http.Request) error {
	if err := s.sameOrigin(r); err != nil {
		return err
	}
	u := s.sessionUser(r)
	if u == nil {
		return errf(http.StatusUnauthorized, "unauthorized", "sign in first")
	}
	name := cleanName(r.FormValue("name"))
	if name == "" {
		return errf(http.StatusBadRequest, "bad_request", "name cannot be empty")
	}
	if _, err := s.db.Exec(r.Context(), `UPDATE users SET name = $2 WHERE id = $1`, u.ID, name); err != nil {
		return err
	}
	http.Redirect(w, r, "/account", http.StatusSeeOther)
	return nil
}

func (s *Server) handleAccountDelete(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	if err := s.sameOrigin(r); err != nil {
		return err
	}
	u := s.sessionUser(r)
	if u == nil {
		return errf(http.StatusUnauthorized, "unauthorized", "sign in first")
	}
	if r.FormValue("confirm") != "delete" {
		return errf(http.StatusBadRequest, "bad_request", "type delete to confirm")
	}
	// Collect the videos this user voted on so their consensus is redone
	// without them once the account (and its votes) are gone.
	rows, err := s.db.Query(ctx, `SELECT video_id FROM assessments WHERE user_id = $1`, u.ID)
	if err != nil {
		return err
	}
	videos, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	if _, err := s.db.Exec(ctx, `DELETE FROM users WHERE id = $1`, u.ID); err != nil {
		return err
	}
	for _, v := range videos {
		if err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error { return s.recomputeVideo(ctx, tx, v) }); err != nil {
			s.log.Error("recompute after account deletion", "video", v, "err", err)
		}
	}
	s.endSession(ctx, w, r)
	http.Redirect(w, r, "/", http.StatusSeeOther)
	return nil
}

// handleContest files an objection from someone without an account, such
// as the creator of a flagged channel. It never changes any status itself.
func (s *Server) handleContest(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	if err := s.sameOrigin(r); err != nil {
		return err
	}
	var req struct {
		Target, Reason, Contact, Challenge, Nonce string
	}
	if err := readJSON(w, r, &req); err != nil {
		return err
	}
	req.Target, req.Reason, req.Contact = strings.TrimSpace(req.Target), strings.TrimSpace(req.Reason), strings.TrimSpace(req.Contact)
	if req.Target == "" || len(req.Target) > 300 || len(req.Reason) < 20 || len(req.Reason) > 4000 || len(req.Contact) > 200 {
		return errf(http.StatusBadRequest, "bad_request", "give the channel or video and a reason of at least 20 characters")
	}
	if err := s.rateLimit(ctx, r, "contest", 1, 5, 24*time.Hour); err != nil {
		return err
	}
	if err := s.checkPow(ctx, req.Challenge, req.Nonce); err != nil {
		return err
	}
	if _, err := s.db.Exec(ctx,
		`INSERT INTO contests (target, reason, contact, net_key, created_at) VALUES ($1, $2, $3, $4, $5)`,
		req.Target, req.Reason, req.Contact, s.netKey(ctx, r), s.now()); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	return nil
}

// ------------------------------------------------------------------ admin

func (s *Server) adminUser(r *http.Request, write bool) (*User, error) {
	u := s.sessionUser(r)
	if u == nil || !u.IsAdmin {
		return nil, errf(http.StatusNotFound, "not_found", "not found")
	}
	if write {
		if err := s.sameOrigin(r); err != nil {
			return nil, err
		}
	}
	return u, nil
}

// handleAdminClaim lets the first signed-in person holding ADMIN_TOKEN
// become the administrator and the seed of trust.
func (s *Server) handleAdminClaim(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	u := s.sessionUser(r)
	if u == nil {
		http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
		return nil
	}
	tok := r.URL.Query().Get("token")
	if s.cfg.AdminToken == "" || subtle.ConstantTimeCompare([]byte(tok), []byte(s.cfg.AdminToken)) != 1 {
		return errf(http.StatusNotFound, "not_found", "not found")
	}
	tag, err := s.db.Exec(ctx,
		`UPDATE users SET is_admin = true, reputation = $2
		  WHERE id = $1 AND NOT EXISTS (SELECT 1 FROM users WHERE is_admin)`, u.ID, repMax)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errf(http.StatusConflict, "conflict", "an administrator already exists")
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
	return nil
}

type adminVideo struct {
	ID, UC             string
	Status, Types, Lvl int
	Yes, No            float64
	NYes, NNo, Ovr     int
	Updated            time.Time
}

type adminChannel struct {
	UC, Handle, Name string
	Flagged          bool
	Ovr, NC, NR      int
}

type adminUserRow struct {
	ID, Name   string
	Reputation float64
	IsAdmin    bool
	Banned     bool
	Reports    int
	Created    time.Time
}

type adminHold struct {
	Kind, ID, Reason string
	At               *time.Time
}

type adminContest struct {
	ID                      int64
	Target, Reason, Contact string
	Created                 time.Time
}

func (s *Server) handleAdmin(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	u, err := s.adminUser(r, false)
	if err != nil {
		return err
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))

	vq := `SELECT id, uc, status, types, level, yes, no, n_yes, n_no, override, updated_at FROM videos `
	var vargs []any
	if q != "" {
		vq += `WHERE id = $1 OR uc = $1 `
		vargs = append(vargs, q)
	} else {
		vq += `WHERE status = 0 OR n_no > 0 OR override <> 0 `
	}
	rows, err := s.db.Query(ctx, vq+`ORDER BY updated_at DESC LIMIT 100`, vargs...)
	if err != nil {
		return err
	}
	videos, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (adminVideo, error) {
		var v adminVideo
		err := row.Scan(&v.ID, &v.UC, &v.Status, &v.Types, &v.Lvl, &v.Yes, &v.No, &v.NYes, &v.NNo, &v.Ovr, &v.Updated)
		return v, err
	})
	if err != nil {
		return err
	}

	cq := `SELECT uc, COALESCE(handle, ''), name, flagged, override, n_confirmed, n_rejected FROM channels `
	var cargs []any
	if q != "" {
		cq += `WHERE uc = $1 OR handle = $2 `
		cargs = append(cargs, q, normHandle(q))
	} else {
		cq += `WHERE flagged OR override <> 0 OR n_confirmed > 0 `
	}
	rows, err = s.db.Query(ctx, cq+`ORDER BY updated_at DESC LIMIT 100`, cargs...)
	if err != nil {
		return err
	}
	channels, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (adminChannel, error) {
		var c adminChannel
		err := row.Scan(&c.UC, &c.Handle, &c.Name, &c.Flagged, &c.Ovr, &c.NC, &c.NR)
		return c, err
	})
	if err != nil {
		return err
	}

	rows, err = s.db.Query(ctx,
		`SELECT u.id::text, u.name, u.reputation, u.is_admin, u.banned,
		        (SELECT count(*) FROM assessments a WHERE a.user_id = u.id), u.created_at
		   FROM users u ORDER BY u.reputation DESC, u.created_at DESC LIMIT 200`)
	if err != nil {
		return err
	}
	users, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (adminUserRow, error) {
		var x adminUserRow
		err := row.Scan(&x.ID, &x.Name, &x.Reputation, &x.IsAdmin, &x.Banned, &x.Reports, &x.Created)
		return x, err
	})
	if err != nil {
		return err
	}

	rows, err = s.db.Query(ctx,
		`SELECT id, target, reason, contact, created_at FROM contests WHERE status = 'open' ORDER BY id LIMIT 100`)
	if err != nil {
		return err
	}
	contests, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (adminContest, error) {
		var c adminContest
		err := row.Scan(&c.ID, &c.Target, &c.Reason, &c.Contact, &c.Created)
		return c, err
	})
	if err != nil {
		return err
	}

	rows, err = s.db.Query(ctx,
		`SELECT 'channel', uc, hold_reason, held_at FROM channels WHERE held
		 UNION ALL
		 SELECT 'video', id, hold_reason, held_at FROM videos WHERE held
		 ORDER BY 4 DESC NULLS LAST LIMIT 100`)
	if err != nil {
		return err
	}
	holds, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (adminHold, error) {
		var h adminHold
		err := row.Scan(&h.Kind, &h.ID, &h.Reason, &h.At)
		return h, err
	})
	if err != nil {
		return err
	}

	var stats struct{ Users, Videos, Confirmed, Flagged, Mapped int }
	if err := s.db.QueryRow(ctx,
		`SELECT (SELECT count(*) FROM users), (SELECT count(*) FROM videos),
		        (SELECT count(*) FROM videos WHERE status = 1),
		        (SELECT count(*) FROM channels WHERE flagged),
		        (SELECT count(*) FROM channels WHERE handle IS NOT NULL)`).
		Scan(&stats.Users, &stats.Videos, &stats.Confirmed, &stats.Flagged, &stats.Mapped); err != nil {
		return err
	}
	return s.page(w, r, http.StatusOK, "admin", map[string]any{
		"User": u, "Q": q, "Videos": videos, "Channels": channels, "Users": users, "Contests": contests, "Holds": holds, "Stats": stats,
	})
}

func overrideValue(s string) (int, bool) {
	switch s {
	case "confirm", "flag":
		return 1, true
	case "reject", "clear":
		return -1, true
	case "auto":
		return 0, true
	}
	return 0, false
}

func (s *Server) handleAdminVideo(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	if _, err := s.adminUser(r, true); err != nil {
		return err
	}
	id := strings.TrimSpace(r.FormValue("id"))
	ovr, ok := overrideValue(r.FormValue("action"))
	if !reVideo.MatchString(id) || !ok {
		return errf(http.StatusBadRequest, "bad_request", "invalid video or action")
	}
	uc := strings.TrimSpace(r.FormValue("uc"))
	if uc != "" && !reUC.MatchString(uc) {
		return errf(http.StatusBadRequest, "bad_request", "invalid channel id")
	}
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`INSERT INTO videos (id, uc, override) VALUES ($1, $2, $3)
			 ON CONFLICT (id) DO UPDATE SET override = EXCLUDED.override,
			   uc = CASE WHEN EXCLUDED.uc <> '' THEN EXCLUDED.uc ELSE videos.uc END`, id, uc, ovr); err != nil {
			return err
		}
		return s.recomputeVideo(ctx, tx, id)
	})
	if err != nil {
		return err
	}
	http.Redirect(w, r, "/admin?q="+id, http.StatusSeeOther)
	return nil
}

func (s *Server) handleAdminChannel(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	if _, err := s.adminUser(r, true); err != nil {
		return err
	}
	uc := strings.TrimSpace(r.FormValue("uc"))
	ovr, ok := overrideValue(r.FormValue("action"))
	if !reUC.MatchString(uc) || !ok {
		return errf(http.StatusBadRequest, "bad_request", "invalid channel or action")
	}
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`INSERT INTO channels (uc, override) VALUES ($1, $2)
			 ON CONFLICT (uc) DO UPDATE SET override = EXCLUDED.override`, uc, ovr); err != nil {
			return err
		}
		return s.recomputeChannel(ctx, tx, uc)
	})
	if err != nil {
		return err
	}
	http.Redirect(w, r, "/admin?q="+uc, http.StatusSeeOther)
	return nil
}

func (s *Server) handleAdminUser(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	if _, err := s.adminUser(r, true); err != nil {
		return err
	}
	id := r.FormValue("id")
	switch r.FormValue("action") {
	case "ban", "unban":
		if _, err := s.db.Exec(ctx, `UPDATE users SET banned = $2 WHERE id = $1 AND NOT is_admin`, id, r.FormValue("action") == "ban"); err != nil {
			return err
		}
		// A ban removes the user's votes from every unsettled video at once.
		if _, err := s.db.Exec(ctx, `DELETE FROM tokens WHERE user_id = $1`, id); err != nil {
			return err
		}
		if err := s.RecomputePending(ctx, 2000); err != nil {
			return err
		}
	case "reputation":
		rep, err := strconv.ParseFloat(r.FormValue("value"), 64)
		if err != nil || rep < 0 || rep > repMax {
			return errf(http.StatusBadRequest, "bad_request", "reputation must be 0..100")
		}
		var old float64
		if err := s.db.QueryRow(ctx, `SELECT reputation FROM users WHERE id = $1`, id).Scan(&old); err != nil {
			return errf(http.StatusBadRequest, "bad_request", "unknown user")
		}
		if _, err := s.db.Exec(ctx, `UPDATE users SET reputation = $2 WHERE id = $1`, id, rep); err != nil {
			return err
		}
		if _, err := s.db.Exec(ctx,
			`INSERT INTO rep_events (user_id, delta, reason, reversed, created_at) VALUES ($1, $2, 'set by administrator', true, $3)`,
			id, rep-old, s.now()); err != nil {
			return err
		}
	default:
		return errf(http.StatusBadRequest, "bad_request", "unknown action")
	}
	http.Redirect(w, r, "/admin#users", http.StatusSeeOther)
	return nil
}

func (s *Server) handleAdminContest(w http.ResponseWriter, r *http.Request) error {
	if _, err := s.adminUser(r, true); err != nil {
		return err
	}
	if _, err := s.db.Exec(r.Context(), `UPDATE contests SET status = 'closed' WHERE id = $1`, r.FormValue("id")); err != nil {
		return err
	}
	http.Redirect(w, r, "/admin#contests", http.StatusSeeOther)
	return nil
}

// handleAdminHold releases a brigading hold: consensus applies again, and
// no new hold starts on the same target for a while.
func (s *Server) handleAdminHold(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	if _, err := s.adminUser(r, true); err != nil {
		return err
	}
	kind, id := r.FormValue("kind"), strings.TrimSpace(r.FormValue("id"))
	if err := s.releaseHold(ctx, kind, id); err != nil {
		return err
	}
	http.Redirect(w, r, "/admin?q="+url.QueryEscape(id), http.StatusSeeOther)
	return nil
}

func (s *Server) releaseHold(ctx context.Context, kind, id string) error {
	now := s.now()
	return pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		switch {
		case kind == "video" && reVideo.MatchString(id):
			if _, err := tx.Exec(ctx,
				`UPDATE videos SET held = false, hold_released_at = $2 WHERE id = $1`, id, now); err != nil {
				return err
			}
			return s.recomputeVideo(ctx, tx, id)
		case kind == "channel" && reUC.MatchString(id):
			if _, err := tx.Exec(ctx,
				`UPDATE channels SET held = false, hold_released_at = $2 WHERE uc = $1`, id, now); err != nil {
				return err
			}
			rows, err := tx.Query(ctx, `SELECT id FROM videos WHERE uc = $1 AND status = 0 ORDER BY id`, id)
			if err != nil {
				return err
			}
			videos, err := pgx.CollectRows(rows, pgx.RowTo[string])
			if err != nil {
				return err
			}
			for _, v := range videos {
				if err := s.recomputeVideo(ctx, tx, v); err != nil {
					return err
				}
			}
			return s.recomputeChannel(ctx, tx, id)
		}
		return errf(http.StatusBadRequest, "bad_request", "invalid hold")
	})
}
