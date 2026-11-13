package server

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/text/unicode/norm"
)

var (
	reUC    = regexp.MustCompile(`^UC[\w-]{22}$`)
	reVideo = regexp.MustCompile(`^[\w-]{11}$`)
)

const (
	claimQuorum      = 3.0 // weight needed before a mapping is served
	claimWeightNet   = 1.0 // an anonymous or low-reputation source, per network
	claimWeightAcct  = 1.5 // an account with established reputation
	claimAcctMinRep  = 5.0
	claimLifetime    = 30 * 24 * time.Hour
	maxBatch         = 200
	maxHandleLen     = 64
	maxChannelNameLn = 120
)

// normHandle lower-cases and NFC-normalises a handle and ensures the leading
// '@'. An empty result means "this channel has no handle".
func normHandle(h string) string {
	h = strings.TrimSpace(h)
	h = strings.TrimPrefix(h, "@")
	if h == "" {
		return ""
	}
	return "@" + strings.ToLower(norm.NFC.String(h))
}

func validHandle(h string) bool {
	if h == "" {
		return true
	}
	if len(h) > maxHandleLen || strings.ContainsAny(h, "/?#\\ \t\r\n") {
		return false
	}
	return strings.HasPrefix(h, "@") && len(h) > 1
}

type channelClaim struct {
	UC     string `json:"uc"`
	Handle string `json:"h"`
	Name   string `json:"name"`
}

// claimSource decides what counts as one independent source. Accounts only
// count on their own once they have earned reputation; everything else is
// grouped by network so one machine cannot pose as many.
func claimSource(u *User, netKey string) (string, float32) {
	if u != nil && !u.Banned && u.Reputation >= claimAcctMinRep {
		return "u:" + u.ID, claimWeightAcct
	}
	return "n:" + netKey, claimWeightNet
}

// SubmitClaims records what a client observed on YouTube and re-evaluates
// each mapping. It returns how many claims were accepted.
func (s *Server) SubmitClaims(ctx context.Context, source string, weight float32, claims []channelClaim) (int, error) {
	n := 0
	for _, c := range claims {
		h := normHandle(c.Handle)
		if !reUC.MatchString(c.UC) || !validHandle(h) {
			continue
		}
		name := strings.TrimSpace(c.Name)
		if len(name) > maxChannelNameLn {
			name = name[:maxChannelNameLn]
		}
		if err := s.applyClaim(ctx, c.UC, source, h, name, weight); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

func (s *Server) applyClaim(ctx context.Context, uc, source, handle, name string, weight float32) error {
	now := s.now()
	return pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`INSERT INTO channels (uc) VALUES ($1) ON CONFLICT DO NOTHING`, uc); err != nil {
			return err
		}
		// Lock the channel so concurrent claims are evaluated one at a time.
		var cur *string
		var flagged bool
		if err := tx.QueryRow(ctx,
			`SELECT handle, flagged FROM channels WHERE uc = $1 FOR UPDATE`, uc).Scan(&cur, &flagged); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO channel_claims (uc, source, handle, name, weight, seen_at)
			 VALUES ($1, $2, $3, $4, $5, $6)
			 ON CONFLICT (uc, source) DO UPDATE
			   SET handle = EXCLUDED.handle, name = EXCLUDED.name,
			       weight = EXCLUDED.weight, seen_at = EXCLUDED.seen_at`,
			uc, source, handle, name, weight, now); err != nil {
			return err
		}

		rows, err := tx.Query(ctx,
			`SELECT handle, sum(weight)::float8, (array_agg(name ORDER BY seen_at DESC))[1]
			   FROM channel_claims
			  WHERE uc = $1 AND seen_at > $2
			  GROUP BY handle ORDER BY 2 DESC`, uc, now.Add(-claimLifetime))
		if err != nil {
			return err
		}
		type tally struct {
			handle, name string
			w            float64
		}
		var ts []tally
		var total float64
		for rows.Next() {
			var t tally
			if err := rows.Scan(&t.handle, &t.w, &t.name); err != nil {
				rows.Close()
				return err
			}
			ts = append(ts, t)
			total += t.w
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}

		next := cur
		nextName := ""
		var second float64
		if len(ts) > 1 {
			second = ts[1].w
		}
		switch {
		case len(ts) > 0 && ts[0].w >= claimQuorum && ts[0].w >= 2*second:
			// A clear winner among independent sources.
			next, nextName = &ts[0].handle, ts[0].name
		case cur != nil:
			// Keep serving the confirmed mapping until enough independent
			// sources say otherwise; then withdraw it until one answer wins.
			var forCur float64
			for _, t := range ts {
				if t.handle == *cur {
					forCur = t.w
				}
			}
			if against := total - forCur; against >= claimQuorum && against > forCur {
				next = nil
			}
		}

		changed := (cur == nil) != (next == nil) || (cur != nil && next != nil && *cur != *next)
		if !changed {
			return nil
		}
		// Only flagged channels are in the published list, so only those
		// need a new sequence number when their handle changes.
		q := `UPDATE channels SET handle = $2, name = CASE WHEN $3 <> '' THEN $3 ELSE name END, updated_at = $4`
		if flagged {
			q += `, seq = nextval('change_seq')`
		}
		_, err = tx.Exec(ctx, q+` WHERE uc = $1`, uc, next, nextName, now)
		return err
	})
}

type channelInfo struct {
	Handle string `json:"h"`
	Name   string `json:"name,omitempty"`
}

// LookupChannels returns the confirmed mappings among ids.
func (s *Server) LookupChannels(ctx context.Context, ids []string) (map[string]channelInfo, error) {
	valid := make([]string, 0, len(ids))
	for _, id := range ids {
		if reUC.MatchString(id) {
			valid = append(valid, id)
		}
	}
	out := map[string]channelInfo{}
	if len(valid) == 0 {
		return out, nil
	}
	rows, err := s.db.Query(ctx,
		`SELECT uc, handle, name FROM channels WHERE uc = ANY($1) AND handle IS NOT NULL`, valid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var uc string
		var ci channelInfo
		if err := rows.Scan(&uc, &ci.Handle, &ci.Name); err != nil {
			return nil, err
		}
		out[uc] = ci
	}
	return out, rows.Err()
}

// ------------------------------------------------------------------ HTTP

func (s *Server) handleLookup(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		IDs []string `json:"ids"`
	}
	if err := readJSON(w, r, &req); err != nil {
		return err
	}
	if len(req.IDs) == 0 || len(req.IDs) > maxBatch {
		return errf(http.StatusBadRequest, "bad_request", "send 1 to %d ids", maxBatch)
	}
	if err := s.rateLimit(r.Context(), r, "lookup", len(req.IDs), 20000, time.Hour); err != nil {
		return err
	}
	found, err := s.LookupChannels(r.Context(), req.IDs)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"found": found})
	return nil
}

func (s *Server) handleClaims(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		Claims []channelClaim `json:"claims"`
	}
	if err := readJSON(w, r, &req); err != nil {
		return err
	}
	if len(req.Claims) == 0 || len(req.Claims) > maxBatch {
		return errf(http.StatusBadRequest, "bad_request", "send 1 to %d claims", maxBatch)
	}
	if err := s.rateLimit(r.Context(), r, "claims", len(req.Claims), 3000, time.Hour); err != nil {
		return err
	}
	// Signing in is optional here; a bad token just means "anonymous".
	u, _ := s.bearerUser(r)
	source, weight := claimSource(u, s.netKey(r.Context(), r))
	n, err := s.SubmitClaims(r.Context(), source, weight, req.Claims)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"accepted": n})
	return nil
}
