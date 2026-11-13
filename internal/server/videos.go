package server

import (
	"context"
	"errors"
	"math"
	"net/http"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
)

// Video status.
const (
	statusPending   = 0
	statusConfirmed = 1
	statusRejected  = 2
)

// Slop types, individually selectable.
const (
	typeVoice  = 1
	typeVisual = 2
	typeScript = 4
	typeMusic  = 8
	typeMask   = 15
)

var typeNames = []struct {
	Bit  int
	Name string
}{{typeVoice, "voice"}, {typeVisual, "visual"}, {typeScript, "script"}, {typeMusic, "music"}}

const (
	repMax         = 100.0
	weightNew      = 0.05 // a brand-new account barely counts
	weightMax      = 3.0
	confirmMargin  = 3.0 // weighted lead needed to settle a video
	minVoters      = 2   // distinct accounts needed on the winning side
	gainFull       = 1.0
	firstReportMul = 1.5
	lossWrong      = 1.5
	gainOverride   = 0.5 // agreeing with an admin ruling
	maxSettle      = 60  // voters considered per side when settling

	chanMinConfirmed = 3
	chanMinReporters = 3
	chanMinShare     = 0.6
)

// voteWeight turns reputation into voting weight. The square root makes the
// first points matter most and keeps veterans from dwarfing everyone else.
func voteWeight(rep float64) float64 {
	if rep < 0 {
		rep = 0
	}
	if rep > repMax {
		rep = repMax
	}
	return weightNew + (weightMax-weightNew)*math.Sqrt(rep/repMax)
}

// dailyReportLimit grows with reputation so new accounts cannot flood.
func dailyReportLimit(rep float64) int {
	n := 5 + int(rep*5)
	if n > 500 {
		n = 500
	}
	return n
}

type vote struct {
	user    string
	slop    bool
	types   int
	level   int
	rep     float64
	created time.Time
}

type outcome struct {
	status    int
	types     int
	level     int
	yes, no   float64
	nYes, nNo int
}

// decide is the pure consensus rule.
func decide(votes []vote, override int) outcome {
	var o outcome
	for _, v := range votes {
		w := voteWeight(v.rep)
		if v.slop {
			o.yes += w
			o.nYes++
		} else {
			o.no += w
			o.nNo++
		}
	}
	switch {
	case override > 0:
		o.status = statusConfirmed
	case override < 0:
		o.status = statusRejected
	case o.yes-o.no >= confirmMargin && o.yes >= 2*o.no && o.nYes >= minVoters:
		o.status = statusConfirmed
	case o.no-o.yes >= confirmMargin && o.no >= 2*o.yes && o.nNo >= minVoters:
		o.status = statusRejected
	}

	// Types: a type sticks when sources holding at least half the "slop"
	// weight selected it. Level: the weighted median.
	type lw struct {
		level int
		w     float64
	}
	var levels []lw
	typeW := map[int]float64{}
	for _, v := range votes {
		if !v.slop {
			continue
		}
		w := voteWeight(v.rep)
		for _, t := range typeNames {
			if v.types&t.Bit != 0 {
				typeW[t.Bit] += w
			}
		}
		if v.level >= 1 && v.level <= 3 {
			levels = append(levels, lw{v.level, w})
		}
	}
	for bit, w := range typeW {
		if o.yes > 0 && w >= o.yes/2 {
			o.types |= bit
		}
	}
	sort.Slice(levels, func(i, j int) bool { return levels[i].level < levels[j].level })
	var total, acc float64
	for _, l := range levels {
		total += l.w
	}
	for _, l := range levels {
		acc += l.w
		if acc >= total/2 {
			o.level = l.level
			break
		}
	}
	return o
}

type assessmentInput struct {
	Video string   `json:"video"`
	UC    string   `json:"uc"`
	Slop  bool     `json:"slop"`
	Types []string `json:"types"`
	Level int      `json:"level"`
}

func typesToMask(names []string) (int, bool) {
	m := 0
	for _, n := range names {
		ok := false
		for _, t := range typeNames {
			if t.Name == n {
				m |= t.Bit
				ok = true
			}
		}
		if !ok {
			return 0, false
		}
	}
	return m, true
}

func maskToTypes(m int) []string {
	out := []string{}
	for _, t := range typeNames {
		if m&t.Bit != 0 {
			out = append(out, t.Name)
		}
	}
	return out
}

// Assess records a user's judgement of a video (report, reinforce or
// contest are all the same record) and re-evaluates the video.
func (s *Server) Assess(ctx context.Context, u *User, in assessmentInput) (*videoState, error) {
	if u.Banned {
		return nil, errf(http.StatusForbidden, "banned", "this account can no longer report")
	}
	if !reVideo.MatchString(in.Video) {
		return nil, errf(http.StatusBadRequest, "bad_request", "invalid video id")
	}
	if in.UC != "" && !reUC.MatchString(in.UC) {
		return nil, errf(http.StatusBadRequest, "bad_request", "invalid channel id")
	}
	mask, ok := typesToMask(in.Types)
	if !ok {
		return nil, errf(http.StatusBadRequest, "bad_request", "types must be any of voice, visual, script, music")
	}
	if in.Slop {
		if mask == 0 {
			return nil, errf(http.StatusBadRequest, "bad_request", "select at least one type of slop")
		}
		if in.Level < 1 || in.Level > 3 {
			return nil, errf(http.StatusBadRequest, "bad_request", "level must be 1, 2 or 3")
		}
	} else {
		mask, in.Level = 0, 0
	}

	now := s.now()
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		// New judgements count against the daily budget; edits do not.
		var exists bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM assessments WHERE user_id = $1 AND video_id = $2)`,
			u.ID, in.Video).Scan(&exists); err != nil {
			return err
		}
		if !exists && !u.IsAdmin {
			var today int
			if err := tx.QueryRow(ctx,
				`SELECT count(*) FROM assessments WHERE user_id = $1 AND created_at > $2`,
				u.ID, now.Add(-24*time.Hour)).Scan(&today); err != nil {
				return err
			}
			if limit := dailyReportLimit(u.Reputation); today >= limit {
				return errf(http.StatusTooManyRequests, "daily_limit",
					"daily limit of %d reached; it grows as your reputation does", limit)
			}
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO videos (id, uc, first_user, created_at, updated_at)
			 VALUES ($1, $2, $3, $4, $4) ON CONFLICT DO NOTHING`,
			in.Video, in.UC, u.ID, now); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO assessments (user_id, video_id, slop, types, level, uc, created_at, updated_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $7)
			 ON CONFLICT (user_id, video_id) DO UPDATE
			   SET slop = EXCLUDED.slop, types = EXCLUDED.types, level = EXCLUDED.level,
			       uc = CASE WHEN EXCLUDED.uc <> '' THEN EXCLUDED.uc ELSE assessments.uc END,
			       updated_at = EXCLUDED.updated_at`,
			u.ID, in.Video, in.Slop, mask, in.Level, in.UC, now); err != nil {
			return err
		}
		return s.recomputeVideo(ctx, tx, in.Video)
	})
	if err != nil {
		return nil, err
	}
	return s.VideoState(ctx, in.Video, u)
}

// recomputeVideo re-runs consensus for one video inside tx, settles
// reputation if the status changed and refreshes the owning channel.
func (s *Server) recomputeVideo(ctx context.Context, tx pgx.Tx, id string) error {
	now := s.now()
	var oldStatus, oldTypes, oldLevel, override int
	var oldUC string
	var firstUser *string
	err := tx.QueryRow(ctx,
		`SELECT status, types, level, override, uc, first_user::text FROM videos WHERE id = $1 FOR UPDATE`, id).
		Scan(&oldStatus, &oldTypes, &oldLevel, &override, &oldUC, &firstUser)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}

	rows, err := tx.Query(ctx,
		`SELECT a.user_id::text, a.slop, a.types, a.level, a.uc, u.reputation, a.created_at
		   FROM assessments a JOIN users u ON u.id = a.user_id
		  WHERE a.video_id = $1 AND NOT u.banned
		  ORDER BY a.created_at`, id)
	if err != nil {
		return err
	}
	var votes []vote
	ucVotes := map[string]float64{}
	for rows.Next() {
		var v vote
		var uc string
		if err := rows.Scan(&v.user, &v.slop, &v.types, &v.level, &uc, &v.rep, &v.created); err != nil {
			rows.Close()
			return err
		}
		votes = append(votes, v)
		if uc != "" {
			ucVotes[uc] += voteWeight(v.rep)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	o := decide(votes, override)
	uc := oldUC
	var best float64
	for k, w := range ucVotes {
		if w > best || (w == best && k < uc) {
			uc, best = k, w
		}
	}

	// A new sequence number whenever the published view of this video
	// changes: it enters, leaves, or changes while confirmed.
	wasPub, isPub := oldStatus == statusConfirmed, o.status == statusConfirmed
	bump := wasPub != isPub || (isPub && (o.types != oldTypes || o.level != oldLevel || uc != oldUC))
	q := `UPDATE videos SET status = $2, types = $3, level = $4, yes = $5, no = $6,
	             n_yes = $7, n_no = $8, uc = $9, updated_at = $10`
	if bump {
		q += `, seq = nextval('change_seq')`
	}
	if _, err := tx.Exec(ctx, q+` WHERE id = $1`,
		id, o.status, o.types, o.level, o.yes, o.no, o.nYes, o.nNo, uc, now); err != nil {
		return err
	}

	if o.status != oldStatus {
		first := ""
		if firstUser != nil {
			first = *firstUser
		}
		if err := s.settle(ctx, tx, id, votes, o.status, override != 0, first); err != nil {
			return err
		}
	}
	// The channel depends on its confirmed videos and on how many different
	// people reported them, so refresh it on any vote on a confirmed video.
	if o.status != oldStatus || uc != oldUC || o.status == statusConfirmed {
		for _, c := range []string{uc, oldUC} {
			if c != "" {
				if err := s.recomputeChannel(ctx, tx, c); err != nil {
					return err
				}
			}
			if uc == oldUC {
				break
			}
		}
	}
	return nil
}

// settle pays out or claws back reputation when a video's status changes.
//
// Winners earn credit from the other winners they agreed with. Credit from
// a partner scales with that partner's own weight (so agreement among
// throwaway accounts is worth nothing) and shrinks each time the same two
// users are rewarded together (so a clique cannot keep feeding itself).
func (s *Server) settle(ctx context.Context, tx pgx.Tx, video string, votes []vote, status int, byAdmin bool, firstUser string) error {
	now := s.now()
	// Undo whatever an earlier outcome awarded for this video.
	if _, err := tx.Exec(ctx,
		`WITH undone AS (
		   UPDATE rep_events SET reversed = true
		    WHERE video_id = $1 AND NOT reversed RETURNING user_id, delta
		 ), sums AS (
		   SELECT user_id, sum(delta) AS d FROM undone GROUP BY user_id
		 )
		 UPDATE users u SET reputation = LEAST($2, GREATEST(0, u.reputation - s.d))
		   FROM sums s WHERE u.id = s.user_id AND NOT u.is_admin`, video, repMax); err != nil {
		return err
	}
	if status == statusPending {
		return nil
	}

	var winners, losers []vote
	for _, v := range votes {
		if v.slop == (status == statusConfirmed) {
			winners = append(winners, v)
		} else {
			losers = append(losers, v)
		}
	}
	if len(winners) > maxSettle {
		winners = winners[:maxSettle]
	}

	// Existing pair counts among the winners.
	ids := make([]string, len(winners))
	for i, v := range winners {
		ids[i] = v.user
	}
	pairs := map[[2]string]int{}
	if len(ids) > 1 {
		rows, err := tx.Query(ctx,
			`SELECT a::text, b::text, n FROM pair_counts WHERE a = ANY($1::uuid[]) AND b = ANY($1::uuid[])`, ids)
		if err != nil {
			return err
		}
		for rows.Next() {
			var a, b string
			var n int
			if err := rows.Scan(&a, &b, &n); err != nil {
				rows.Close()
				return err
			}
			pairs[[2]string{a, b}] = n
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
	}
	pairKey := func(a, b string) [2]string {
		if a > b {
			a, b = b, a
		}
		return [2]string{a, b}
	}

	award := func(user string, delta float64, reason string) error {
		if delta == 0 {
			return nil
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO rep_events (user_id, video_id, delta, reason, created_at) VALUES ($1, $2, $3, $4, $5)`,
			user, video, delta, reason, now); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`UPDATE users SET reputation = LEAST($2, GREATEST(0, reputation + $3)) WHERE id = $1 AND NOT is_admin`,
			user, repMax, delta)
		return err
	}

	for _, v := range winners {
		var gain float64
		if byAdmin {
			gain = gainOverride
		} else {
			var credit float64
			for _, p := range winners {
				if p.user == v.user {
					continue
				}
				credit += voteWeight(p.rep) / weightMax / float64(1+pairs[pairKey(v.user, p.user)])
			}
			gain = gainFull * math.Min(1, credit)
		}
		if status == statusConfirmed && v.user == firstUser {
			gain *= firstReportMul
		}
		if err := award(v.user, gain, "agreed with outcome"); err != nil {
			return err
		}
	}
	for _, v := range losers {
		if err := award(v.user, -lossWrong, "disagreed with outcome"); err != nil {
			return err
		}
	}

	if !byAdmin {
		for i := 0; i < len(winners); i++ {
			for j := i + 1; j < len(winners); j++ {
				k := pairKey(winners[i].user, winners[j].user)
				if _, err := tx.Exec(ctx,
					`INSERT INTO pair_counts (a, b, n) VALUES ($1, $2, 1)
					 ON CONFLICT (a, b) DO UPDATE SET n = pair_counts.n + 1`, k[0], k[1]); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// recomputeChannel flags a channel once enough of its videos are confirmed
// by enough different people, and clears it again if that stops being true.
func (s *Server) recomputeChannel(ctx context.Context, tx pgx.Tx, uc string) error {
	if _, err := tx.Exec(ctx, `INSERT INTO channels (uc) VALUES ($1) ON CONFLICT DO NOTHING`, uc); err != nil {
		return err
	}
	var was bool
	var override int
	if err := tx.QueryRow(ctx,
		`SELECT flagged, override FROM channels WHERE uc = $1 FOR UPDATE`, uc).Scan(&was, &override); err != nil {
		return err
	}
	var nConf, nRej, reporters int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FILTER (WHERE status = 1), count(*) FILTER (WHERE status = 2) FROM videos WHERE uc = $1`,
		uc).Scan(&nConf, &nRej); err != nil {
		return err
	}
	if err := tx.QueryRow(ctx,
		`SELECT count(DISTINCT a.user_id)
		   FROM assessments a JOIN videos v ON v.id = a.video_id JOIN users u ON u.id = a.user_id
		  WHERE v.uc = $1 AND v.status = 1 AND a.slop AND NOT u.banned`, uc).Scan(&reporters); err != nil {
		return err
	}
	flagged := nConf >= chanMinConfirmed && reporters >= chanMinReporters &&
		float64(nConf) >= chanMinShare*float64(nConf+nRej)
	if override > 0 {
		flagged = true
	} else if override < 0 {
		flagged = false
	}
	q := `UPDATE channels SET flagged = $2, n_confirmed = $3, n_rejected = $4, updated_at = $5`
	if flagged != was {
		q += `, seq = nextval('change_seq')`
	}
	_, err := tx.Exec(ctx, q+` WHERE uc = $1`, uc, flagged, nConf, nRej, s.now())
	return err
}

// RecomputePending re-runs consensus for unsettled videos, because voters'
// reputations (and so their weights) move after they vote.
func (s *Server) RecomputePending(ctx context.Context, limit int) error {
	rows, err := s.db.Query(ctx,
		`SELECT id FROM videos WHERE status = 0 AND updated_at > $1 ORDER BY updated_at DESC LIMIT $2`,
		s.now().Add(-60*24*time.Hour), limit)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range ids {
		if err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error { return s.recomputeVideo(ctx, tx, id) }); err != nil {
			return err
		}
	}
	return nil
}

type videoState struct {
	Video  string   `json:"video"`
	Status string   `json:"status"`
	Types  []string `json:"types"`
	Level  int      `json:"level"`
	NYes   int      `json:"n_yes"`
	NNo    int      `json:"n_no"`
	Mine   *struct {
		Slop  bool     `json:"slop"`
		Types []string `json:"types"`
		Level int      `json:"level"`
	} `json:"mine,omitempty"`
}

var statusNames = map[int]string{statusPending: "pending", statusConfirmed: "confirmed", statusRejected: "rejected"}

// VideoState reports where a video stands; u may be nil.
func (s *Server) VideoState(ctx context.Context, id string, u *User) (*videoState, error) {
	vs := &videoState{Video: id, Status: "unknown", Types: []string{}}
	var status, types int
	err := s.db.QueryRow(ctx,
		`SELECT status, types, level, n_yes, n_no FROM videos WHERE id = $1`, id).
		Scan(&status, &types, &vs.Level, &vs.NYes, &vs.NNo)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	if err == nil {
		vs.Status, vs.Types = statusNames[status], maskToTypes(types)
	}
	if u != nil {
		var slop bool
		var mt, ml int
		err := s.db.QueryRow(ctx,
			`SELECT slop, types, level FROM assessments WHERE user_id = $1 AND video_id = $2`, u.ID, id).
			Scan(&slop, &mt, &ml)
		if err == nil {
			vs.Mine = &struct {
				Slop  bool     `json:"slop"`
				Types []string `json:"types"`
				Level int      `json:"level"`
			}{slop, maskToTypes(mt), ml}
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
	}
	return vs, nil
}

// ------------------------------------------------------------------ HTTP

func (s *Server) handleAssess(w http.ResponseWriter, r *http.Request) error {
	u, err := s.bearerUser(r)
	if err != nil {
		return err
	}
	var in assessmentInput
	if err := readJSON(w, r, &in); err != nil {
		return err
	}
	vs, err := s.Assess(r.Context(), u, in)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, vs)
	return nil
}

func (s *Server) handleVideo(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("id")
	if !reVideo.MatchString(id) {
		return errf(http.StatusBadRequest, "bad_request", "invalid video id")
	}
	if err := s.rateLimit(r.Context(), r, "video", 1, 600, time.Hour); err != nil {
		return err
	}
	u, _ := s.bearerUser(r)
	vs, err := s.VideoState(r.Context(), id, u)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, vs)
	return nil
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) error {
	u, err := s.bearerUser(r)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"name":        u.Name,
		"reputation":  math.Round(u.Reputation*100) / 100,
		"weight":      math.Round(voteWeight(u.Reputation)*100) / 100,
		"daily_limit": dailyReportLimit(u.Reputation),
		"admin":       u.IsAdmin,
	})
	return nil
}
