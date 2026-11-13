package server

import (
	"context"
	"errors"
	"fmt"
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
	builderShare   = 0.5 // for voting before the video settled
	followShare    = 0.5 // for agreement that arrives after your vote
	firstReportMul = 1.5 // first reporter: applies to the gain and to the loss
	lossWrong      = 1.5
	overturnMul    = 2.0 // reporters of a listing an administrator overturns
	gainOverride   = 0.5 // agreeing with an admin ruling
	maxSettle      = 150 // winning voters, in order of arrival, who can be paid
	dailyGainCap   = 3.0 // most reputation one account can gain per day

	// Brigading: many recent reports, mostly from new accounts.
	brigadeWindow    = 48 * time.Hour
	brigadeReporters = 6
	brigadeYoung     = 0.6
	youngAge         = 14 * 24 * time.Hour
	youngRep         = 2.0
	holdGrace        = 30 * 24 * time.Hour // no new hold this soon after a release

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
	user       string
	slop       bool
	types      int
	level      int
	rep        float64
	created    time.Time
	net        string    // hashed network the vote came from; "" if unknown
	chanListed bool      // the channel was already listed when the vote was cast
	joined     time.Time // when the account was created
}

// effective drops all but the strongest vote per network on each side, so
// several accounts behind one network count once.
func effective(votes []vote) []vote {
	seen := map[string]int{}
	out := make([]vote, 0, len(votes))
	for _, v := range votes {
		if v.net == "" {
			out = append(out, v)
			continue
		}
		k := v.net + "|n"
		if v.slop {
			k = v.net + "|y"
		}
		if i, ok := seen[k]; ok {
			if v.rep > out[i].rep {
				out[i] = v
			}
			continue
		}
		seen[k] = len(out)
		out = append(out, v)
	}
	return out
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
	votes = effective(votes)
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
	net   string   // hashed network of the caller, set by the handler
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
		// Changing sides counts as a new vote: it goes to the back of the
		// queue, so nobody can vote early and switch to the winning side.
		if _, err := tx.Exec(ctx,
			`INSERT INTO assessments (user_id, video_id, slop, types, level, uc, net_key, chan_listed, created_at, updated_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $8,
			         COALESCE((SELECT flagged FROM channels WHERE uc = $6 AND $6 <> ''), false), $7, $7)
			 ON CONFLICT (user_id, video_id) DO UPDATE
			   SET slop = EXCLUDED.slop, types = EXCLUDED.types, level = EXCLUDED.level,
			       uc = CASE WHEN EXCLUDED.uc <> '' THEN EXCLUDED.uc ELSE assessments.uc END,
			       net_key = EXCLUDED.net_key,
			       chan_listed = CASE WHEN assessments.slop <> EXCLUDED.slop THEN EXCLUDED.chan_listed ELSE assessments.chan_listed END,
			       created_at = CASE WHEN assessments.slop <> EXCLUDED.slop THEN EXCLUDED.created_at ELSE assessments.created_at END,
			       updated_at = EXCLUDED.updated_at`,
			u.ID, in.Video, in.Slop, mask, in.Level, in.UC, now, in.net); err != nil {
			return err
		}
		return s.recomputeVideo(ctx, tx, in.Video)
	})
	if err != nil {
		return nil, err
	}
	return s.VideoState(ctx, in.Video, u)
}

// recomputeVideo re-runs consensus for one video inside tx, brings every
// voter's reputation for it up to date and refreshes the owning channel.
func (s *Server) recomputeVideo(ctx context.Context, tx pgx.Tx, id string) error {
	now := s.now()
	var oldStatus, oldTypes, oldLevel, override int
	var oldUC string
	var held bool
	var released *time.Time
	err := tx.QueryRow(ctx,
		`SELECT status, types, level, override, uc, held, hold_released_at FROM videos WHERE id = $1 FOR UPDATE`, id).
		Scan(&oldStatus, &oldTypes, &oldLevel, &override, &oldUC, &held, &released)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}

	rows, err := tx.Query(ctx,
		`SELECT a.user_id::text, a.slop, a.types, a.level, a.uc, u.reputation, a.created_at,
		        a.net_key, a.chan_listed, u.created_at
		   FROM assessments a JOIN users u ON u.id = a.user_id
		  WHERE a.video_id = $1 AND NOT u.banned
		  ORDER BY a.created_at, a.user_id`, id)
	if err != nil {
		return err
	}
	var votes []vote
	ucVotes := map[string]float64{}
	for rows.Next() {
		var v vote
		var uc string
		if err := rows.Scan(&v.user, &v.slop, &v.types, &v.level, &uc, &v.rep, &v.created,
			&v.net, &v.chanListed, &v.joined); err != nil {
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

	// A hold stops new listings until an administrator has looked; it never
	// removes something that was already listed.
	newHold := false
	if override == 0 {
		chanHeld, reason, err := s.checkHold(ctx, tx, id, uc, votes, held, released)
		if err != nil {
			return err
		}
		if reason != "" {
			newHold = true
			if uc == "" {
				held = true
				if _, err := tx.Exec(ctx,
					`UPDATE videos SET held = true, hold_reason = $2, held_at = $3 WHERE id = $1`, id, reason, now); err != nil {
					return err
				}
			} else {
				chanHeld = true
				if _, err := tx.Exec(ctx,
					`INSERT INTO channels (uc, held, hold_reason, held_at) VALUES ($1, true, $2, $3)
					 ON CONFLICT (uc) DO UPDATE SET held = true, hold_reason = EXCLUDED.hold_reason, held_at = EXCLUDED.held_at`,
					uc, reason, now); err != nil {
					return err
				}
			}
		}
		if (held || chanHeld) && oldStatus != statusConfirmed && o.status == statusConfirmed {
			o.status = statusPending
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

	if err := s.rescore(ctx, tx, id, uc, votes, o.status, override); err != nil {
		return err
	}

	// The channel depends on its confirmed videos and on how many different
	// people reported them, so refresh it on any vote on a confirmed video.
	if o.status != oldStatus || uc != oldUC || o.status == statusConfirmed || newHold {
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

func isYoung(v vote, now time.Time) bool {
	return v.rep < youngRep || now.Sub(v.joined) < youngAge
}

// checkHold reports whether the video's channel is on hold and, if a hold
// should start now, why. A burst of reports that comes mostly from new
// accounts looks like a brigade and waits for an administrator.
func (s *Server) checkHold(ctx context.Context, tx pgx.Tx, id, uc string, votes []vote, videoHeld bool, videoReleased *time.Time) (chanHeld bool, reason string, err error) {
	now := s.now()
	recent := func(t *time.Time) bool { return t != nil && now.Sub(*t) < holdGrace }
	since := now.Add(-brigadeWindow)
	verdict := func(n, young int) string {
		if n >= brigadeReporters && float64(young) >= brigadeYoung*float64(n) {
			return fmt.Sprintf("%d of %d reporters in the last 48 hours are new accounts", young, n)
		}
		return ""
	}

	if uc == "" {
		if videoHeld || recent(videoReleased) {
			return false, "", nil
		}
		var n, young int
		for _, v := range votes {
			if v.slop && v.created.After(since) {
				n++
				if isYoung(v, now) {
					young++
				}
			}
		}
		return false, verdict(n, young), nil
	}

	var flagged bool
	var override int
	var released *time.Time
	err = tx.QueryRow(ctx,
		`SELECT flagged, override, held, hold_released_at FROM channels WHERE uc = $1`, uc).
		Scan(&flagged, &override, &chanHeld, &released)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, "", err
	}
	if chanHeld || flagged || override != 0 || recent(released) {
		return chanHeld, "", nil
	}
	var n, young int
	err = tx.QueryRow(ctx,
		`SELECT count(DISTINCT a.user_id),
		        count(DISTINCT a.user_id) FILTER (WHERE u.reputation < $3 OR u.created_at > $4)
		   FROM assessments a JOIN users u ON u.id = a.user_id
		  WHERE a.uc = $1 AND a.slop AND a.created_at > $2 AND NOT u.banned`,
		uc, since, youngRep, now.Add(-youngAge)).Scan(&n, &young)
	if err != nil {
		return false, "", err
	}
	return false, verdict(n, young), nil
}

func pairKey(a, b string) [2]string {
	if a > b {
		a, b = b, a
	}
	return [2]string{a, b}
}

type scoreInput struct {
	status     int
	byAdmin    bool
	overturned bool              // an administrator rejected what consensus had listed
	pairs      map[[2]string]int // times two users were already rewarded together elsewhere
	chanPrior  map[string]int    // user → earlier judgements on the same channel
}

// score is the pure reputation rule: what each voter on one video should
// have gained or lost for it, given the votes in order of arrival.
//
// A winner is paid two ways. The builder share goes to everyone who voted
// while the video was still undecided. The follower share grows with the
// agreement that arrives after their vote, relative to what was there
// before it, so joining a decided video late pays next to nothing. Both are
// scaled by the weight of the people agreed with (throwaway accounts are
// worth nothing), shrink for partners already rewarded together, and ignore
// partners on the same network.
func score(votes []vote, in scoreInput) map[string]float64 {
	out := map[string]float64{}
	if in.status == statusPending {
		return out
	}
	win := in.status == statusConfirmed
	first := ""
	var yesW float64
	for _, v := range votes {
		if v.slop {
			if first == "" {
				first = v.user
			}
			yesW += voteWeight(v.rep)
		}
	}

	// Where the video stood just before each vote.
	settled := make([]bool, len(votes))
	lead := make([]float64, len(votes))
	var side, other float64
	var nSide int
	for i, v := range votes {
		settled[i] = side-other >= confirmMargin && side >= 2*other && nSide >= minVoters
		lead[i] = side - other
		if v.slop == win {
			side += voteWeight(v.rep)
			nSide++
		} else {
			other += voteWeight(v.rep)
		}
	}

	winners := 0
	for i, v := range votes {
		if v.slop != win {
			loss := lossWrong
			if v.slop {
				if in.overturned {
					loss *= overturnMul
				}
				if v.user == first {
					loss *= firstReportMul
				}
			}
			out[v.user] = -loss
			continue
		}
		winners++
		if winners > maxSettle {
			continue
		}
		gain := gainOverride
		if !in.byAdmin {
			var credit, after, before float64
			for j, p := range votes {
				if j == i || p.slop != win {
					continue
				}
				w := voteWeight(p.rep)
				if j < i {
					before += w
				}
				if v.net != "" && v.net == p.net {
					continue
				}
				w /= float64(1 + in.pairs[pairKey(v.user, p.user)])
				credit += w / weightMax
				if j > i {
					after += w
				}
			}
			gain = 0
			if !settled[i] {
				gain += builderShare * math.Min(1, credit)
			}
			if after > 0 {
				undecided := 1.0
				if lead[i] > confirmMargin {
					undecided = confirmMargin / lead[i]
				}
				gain += followShare * after / (before + after + voteWeight(v.rep)) *
					undecided * math.Min(1, after/weightMax)
			}
			gain *= gainFull
		}
		// "Not slop" only earns when it answers a report that carried weight.
		if !win {
			gain *= math.Min(1, yesW/weightMax)
		}
		// One judgement about a channel should not pay for each of its videos.
		if v.chanListed {
			gain = 0
		} else {
			gain /= float64(1 + in.chanPrior[v.user])
		}
		if win && v.user == first {
			gain *= firstReportMul
		}
		if gain > 0 {
			out[v.user] = gain
		}
	}
	return out
}

// rescore brings the ledger for one video in line with score. It runs on
// every change, so reputation follows later agreement and reverses itself
// when an outcome flips.
func (s *Server) rescore(ctx context.Context, tx pgx.Tx, video, uc string, votes []vote, status, override int) error {
	in := scoreInput{status: status, byAdmin: override != 0, pairs: map[[2]string]int{}, chanPrior: map[string]int{}}
	in.overturned = override < 0 && decide(votes, 0).status == statusConfirmed

	// Winners who can pair up for this video.
	var ids []string
	if status != statusPending && !in.byAdmin {
		for _, v := range votes {
			if v.slop == (status == statusConfirmed) && len(ids) < maxSettle {
				ids = append(ids, v.user)
			}
		}
	}

	// Pairs already recorded for this video, so its own contribution is
	// left out of the history and can be undone.
	recorded := map[[2]string]bool{}
	rows, err := tx.Query(ctx, `SELECT a::text, b::text FROM video_pairs WHERE video_id = $1`, video)
	if err != nil {
		return err
	}
	for rows.Next() {
		var a, b string
		if err := rows.Scan(&a, &b); err != nil {
			rows.Close()
			return err
		}
		recorded[[2]string{a, b}] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

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
			k := [2]string{a, b}
			if recorded[k] {
				n--
			}
			if n > 0 {
				in.pairs[k] = n
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
	}

	if uc != "" && status != statusPending {
		rows, err := tx.Query(ctx,
			`SELECT me.user_id::text, count(*)
			   FROM assessments me
			   JOIN assessments a ON a.user_id = me.user_id AND a.video_id <> me.video_id
			                     AND a.uc = $2 AND a.created_at < me.created_at
			  WHERE me.video_id = $1 GROUP BY 1`, video, uc)
		if err != nil {
			return err
		}
		for rows.Next() {
			var u string
			var n int
			if err := rows.Scan(&u, &n); err != nil {
				rows.Close()
				return err
			}
			in.chanPrior[u] = n
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
	}

	if err := s.applyScores(ctx, tx, video, score(votes, in)); err != nil {
		return err
	}

	// Record which pairs this video rewarded; forget those it no longer does.
	want := map[[2]string]bool{}
	for i := 0; i < len(ids); i++ {
		for j := i + 1; j < len(ids); j++ {
			want[pairKey(ids[i], ids[j])] = true
		}
	}
	var addA, addB, delA, delB []string
	for k := range want {
		if !recorded[k] {
			addA, addB = append(addA, k[0]), append(addB, k[1])
		}
	}
	for k := range recorded {
		if !want[k] {
			delA, delB = append(delA, k[0]), append(delB, k[1])
		}
	}
	if len(addA) > 0 {
		if _, err := tx.Exec(ctx,
			`INSERT INTO video_pairs (video_id, a, b)
			 SELECT $1, t.a, t.b FROM unnest($2::uuid[], $3::uuid[]) AS t(a, b)`, video, addA, addB); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO pair_counts (a, b, n)
			 SELECT t.a, t.b, 1 FROM unnest($1::uuid[], $2::uuid[]) AS t(a, b) ORDER BY 1, 2
			 ON CONFLICT (a, b) DO UPDATE SET n = pair_counts.n + 1`, addA, addB); err != nil {
			return err
		}
	}
	if len(delA) > 0 {
		if _, err := tx.Exec(ctx,
			`DELETE FROM video_pairs v USING unnest($2::uuid[], $3::uuid[]) AS t(a, b)
			  WHERE v.video_id = $1 AND v.a = t.a AND v.b = t.b`, video, delA, delB); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE pair_counts p SET n = GREATEST(0, p.n - 1)
			   FROM unnest($1::uuid[], $2::uuid[]) AS t(a, b) WHERE p.a = t.a AND p.b = t.b`, delA, delB); err != nil {
			return err
		}
	}
	return nil
}

// applyScores moves each voter's reputation to what the video is now worth
// to them. Each user has at most one live ledger row per video; "applied"
// is what actually reached their reputation, which the daily cap can hold
// below the target until a later rescore.
func (s *Server) applyScores(ctx context.Context, tx pgx.Tx, video string, scores map[string]float64) error {
	now := s.now()
	type live struct {
		id      int64
		applied float64
	}
	old := map[string]live{}
	rows, err := tx.Query(ctx,
		`SELECT id, user_id::text, applied FROM rep_events WHERE video_id = $1 AND NOT reversed`, video)
	if err != nil {
		return err
	}
	for rows.Next() {
		var l live
		var u string
		if err := rows.Scan(&l.id, &u, &l.applied); err != nil {
			rows.Close()
			return err
		}
		old[u] = l
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	// Always touch users in the same order so concurrent videos cannot deadlock.
	users := make([]string, 0, len(scores)+len(old))
	for u := range scores {
		users = append(users, u)
	}
	for u := range old {
		if _, ok := scores[u]; !ok {
			users = append(users, u)
		}
	}
	sort.Strings(users)

	bump := func(user string, by float64) error {
		if by == 0 {
			return nil
		}
		_, err := tx.Exec(ctx,
			`UPDATE users SET reputation = LEAST($2, GREATEST(0, reputation + $3)) WHERE id = $1 AND NOT is_admin`,
			user, repMax, by)
		return err
	}

	for _, u := range users {
		l, had := old[u]
		target, scored := scores[u]
		if !scored {
			if _, err := tx.Exec(ctx, `UPDATE rep_events SET reversed = true WHERE id = $1`, l.id); err != nil {
				return err
			}
			if err := bump(u, -l.applied); err != nil {
				return err
			}
			continue
		}
		change := target - l.applied
		if change > 1e-9 {
			if change, err = s.grantGain(ctx, tx, u, change, now); err != nil {
				return err
			}
		} else if change > -1e-9 {
			change = 0
		}
		applied := l.applied + change
		reason := "agreed with outcome"
		if target < 0 {
			reason = "disagreed with outcome"
		}
		if had {
			if _, err := tx.Exec(ctx,
				`UPDATE rep_events SET delta = $2, applied = $3, reason = $4 WHERE id = $1`,
				l.id, target, applied, reason); err != nil {
				return err
			}
		} else if _, err := tx.Exec(ctx,
			`INSERT INTO rep_events (user_id, video_id, delta, applied, reason, created_at) VALUES ($1, $2, $3, $4, $5, $6)`,
			u, video, target, applied, reason, now); err != nil {
			return err
		}
		if err := bump(u, change); err != nil {
			return err
		}
	}
	return nil
}

// grantGain returns how much of a wanted gain fits under the user's daily
// cap and records it as used.
func (s *Server) grantGain(ctx context.Context, tx pgx.Tx, user string, want float64, now time.Time) (float64, error) {
	day := now.UTC().Format("2006-01-02")
	var used float64
	err := tx.QueryRow(ctx,
		`SELECT CASE WHEN gain_day = $2::date THEN gain_used ELSE 0 END FROM users WHERE id = $1 FOR UPDATE`,
		user, day).Scan(&used)
	if err != nil {
		return 0, err
	}
	grant := math.Min(want, dailyGainCap-used)
	if grant <= 0 {
		return 0, nil
	}
	_, err = tx.Exec(ctx, `UPDATE users SET gain_day = $2::date, gain_used = $3 WHERE id = $1`, user, day, used+grant)
	return grant, err
}

// recomputeChannel flags a channel once enough of its videos are confirmed
// by enough different people, and clears it again if that stops being true.
func (s *Server) recomputeChannel(ctx context.Context, tx pgx.Tx, uc string) error {
	if _, err := tx.Exec(ctx, `INSERT INTO channels (uc) VALUES ($1) ON CONFLICT DO NOTHING`, uc); err != nil {
		return err
	}
	var was, held bool
	var override int
	if err := tx.QueryRow(ctx,
		`SELECT flagged, override, held FROM channels WHERE uc = $1 FOR UPDATE`, uc).Scan(&was, &override, &held); err != nil {
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
	if held && !was {
		flagged = false // on hold: no new listing until an administrator releases it
	}
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
	// Counts on an undecided video are withheld until the caller has voted,
	// so there is no leading side to copy.
	CountsHidden bool `json:"counts_hidden,omitempty"`
	Mine         *struct {
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
	if vs.Status == "pending" && vs.Mine == nil {
		vs.NYes, vs.NNo, vs.CountsHidden = 0, 0, true
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
	if k := s.netKey(r.Context(), r); k != "unknown" {
		in.net = k
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
