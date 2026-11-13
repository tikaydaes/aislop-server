package server

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// These tests need a real Postgres: set TEST_DATABASE_URL to an empty
// database the tests may wipe.

func newTestServer(t *testing.T) *Server {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	cfg := Config{
		DatabaseURL:  dsn,
		Origin:       "http://localhost:8080",
		RedirectURIs: []string{"https://ext.example/cb"},
		AdminToken:   "admin-secret",
		PowBits:      4,
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	// Start from an empty schema every time.
	boot, err := New(ctx, cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := boot.db.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public`); err != nil {
		t.Fatal(err)
	}
	boot.Close()
	s, err := New(ctx, cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

var userN int

func mkUser(t *testing.T, s *Server, rep float64) *User {
	t.Helper()
	userN++
	u := &User{Name: fmt.Sprintf("user%d", userN), Reputation: rep}
	err := s.db.QueryRow(context.Background(),
		`INSERT INTO users (wa_handle, name, reputation, created_at) VALUES ($1, $2, $3, now() - interval '90 days') RETURNING id::text`,
		[]byte(fmt.Sprintf("handle-%d-%d", userN, time.Now().UnixNano())), u.Name, rep).Scan(&u.ID)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// mkNewUser is an account that signed up just now.
func mkNewUser(t *testing.T, s *Server, rep float64) *User {
	t.Helper()
	u := mkUser(t, s, rep)
	if _, err := s.db.Exec(context.Background(), `UPDATE users SET created_at = now() WHERE id = $1`, u.ID); err != nil {
		t.Fatal(err)
	}
	return u
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

func rep(t *testing.T, s *Server, u *User) float64 {
	t.Helper()
	var r float64
	if err := s.db.QueryRow(context.Background(), `SELECT reputation FROM users WHERE id = $1`, u.ID).Scan(&r); err != nil {
		t.Fatal(err)
	}
	return r
}

// fresh reloads a user so its reputation is current, as a real request would.
func fresh(t *testing.T, s *Server, u *User) *User {
	t.Helper()
	f, err := s.userByID(context.Background(), u.ID)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func vid(n int) string { return fmt.Sprintf("vid%08d", n) }
func chn(n int) string { return fmt.Sprintf("UC%022d", n) }

func assess(t *testing.T, s *Server, u *User, video, uc string, slop bool) *videoState {
	t.Helper()
	in := assessmentInput{Video: video, UC: uc, Slop: slop}
	if slop {
		in.Types, in.Level = []string{"voice", "visual"}, 2
	}
	vs, err := s.Assess(context.Background(), fresh(t, s, u), in)
	if err != nil {
		t.Fatalf("assess %s by %s: %v", video, u.Name, err)
	}
	return vs
}

func TestVoteWeight(t *testing.T) {
	if w := voteWeight(0); w != weightNew {
		t.Errorf("new account weight = %v", w)
	}
	if w := voteWeight(100); w != weightMax {
		t.Errorf("max weight = %v", w)
	}
	if voteWeight(10) <= voteWeight(1) || voteWeight(1000) != weightMax || voteWeight(-5) != weightNew {
		t.Error("weight must be monotonic and clamped")
	}
}

func TestDecide(t *testing.T) {
	v := func(slop bool, rep float64, types, level int) vote {
		return vote{slop: slop, rep: rep, types: types, level: level}
	}

	// One heavy voter is not enough: two distinct accounts are required.
	if o := decide([]vote{v(true, 100, 1, 3)}, 0); o.status != statusPending {
		t.Errorf("single voter settled a video: %+v", o)
	}
	// Two established voters confirm. Only "voice" has at least half the
	// weight behind it; the level is the weighted median.
	o := decide([]vote{v(true, 100, typeVoice|typeVisual, 3), v(true, 100, typeVoice, 3), v(true, 1, typeMusic, 1)}, 0)
	if o.status != statusConfirmed || o.types != typeVoice || o.level != 3 {
		t.Errorf("confirm: %+v", o)
	}
	// Many throwaway accounts cannot outvote that.
	votes := []vote{v(true, 100, 1, 2), v(true, 100, 1, 2)}
	for i := 0; i < 30; i++ {
		votes = append(votes, v(false, 0, 0, 0))
	}
	if o := decide(votes, 0); o.status != statusConfirmed {
		t.Errorf("30 new accounts overturned two established ones: %+v", o)
	}
	// A real dispute stays pending.
	if o := decide([]vote{v(true, 100, 1, 2), v(true, 100, 1, 2), v(false, 100, 0, 0), v(false, 100, 0, 0)}, 0); o.status != statusPending {
		t.Errorf("tie settled: %+v", o)
	}
	// Admin ruling wins regardless.
	if o := decide([]vote{v(true, 100, 1, 2), v(true, 100, 1, 2)}, -1); o.status != statusRejected {
		t.Errorf("override ignored: %+v", o)
	}
}

func TestConsensusAndReputation(t *testing.T) {
	s := newTestServer(t)
	a, b, c := mkUser(t, s, 80), mkUser(t, s, 80), mkUser(t, s, 80)
	wrong := mkUser(t, s, 5)

	if vs := assess(t, s, a, vid(1), chn(1), true); vs.Status != "pending" {
		t.Fatalf("after one report: %s", vs.Status)
	}
	assess(t, s, wrong, vid(1), chn(1), false)
	vs := assess(t, s, b, vid(1), chn(1), true)
	if vs.Status != "confirmed" || vs.Level != 2 || strings.Join(vs.Types, ",") != "voice,visual" {
		t.Fatalf("after second report: %+v", vs)
	}
	if got := rep(t, s, a); got <= 80 {
		t.Errorf("first reporter did not gain: %v", got)
	}
	if ga, gb := rep(t, s, a)-80, rep(t, s, b)-80; ga <= gb || gb <= 0 {
		t.Errorf("first reporter should gain more than the confirmer, who still gains: %v vs %v", ga, gb)
	}
	if got := rep(t, s, wrong); got != 5-lossWrong {
		t.Errorf("wrong voter reputation = %v", got)
	}

	// The outcome flips: b changes sides and c agrees with the dissenter.
	// Earlier awards are reversed, not stacked, and the first reporter
	// pays the first-reporter price for being wrong.
	assess(t, s, b, vid(1), chn(1), false)
	vs = assess(t, s, c, vid(1), chn(1), false)
	if vs.Status != "rejected" {
		t.Fatalf("after flip: %s", vs.Status)
	}
	if got := rep(t, s, wrong); got <= 5 {
		t.Errorf("the dissenter was right in the end but has %v", got)
	}
	if got := rep(t, s, a); !near(got, 80-lossWrong*firstReportMul) {
		t.Errorf("first reporter after losing = %v", got)
	}
}

func TestScore(t *testing.T) {
	t0 := time.Now()
	n := 0
	v := func(slop bool, rep float64) vote {
		n++
		return vote{user: fmt.Sprintf("u%03d", n), slop: slop, rep: rep, created: t0.Add(time.Duration(n) * time.Second)}
	}
	in := func(status int) scoreInput {
		return scoreInput{status: status, pairs: map[[2]string]int{}, chanPrior: map[string]int{}}
	}

	// Three established voters: the first is paid for building the
	// consensus and for being followed, the second mostly for building,
	// the third arrived after it was decided and with nobody behind them.
	votes := []vote{v(true, 100), v(true, 100), v(true, 100)}
	got := score(votes, in(statusConfirmed))
	if !near(got[votes[0].user], (builderShare+followShare*6/9)*firstReportMul) ||
		!near(got[votes[1].user], builderShare+followShare*3/9) || got[votes[2].user] != 0 {
		t.Errorf("three voters: %v", got)
	}

	// Joining a landslide pays next to nothing, even with a follower.
	votes = nil
	for i := 0; i < 10; i++ {
		votes = append(votes, v(true, 100))
	}
	late := v(true, 100)
	votes = append(votes, late, v(true, 100))
	if g := score(votes, in(statusConfirmed))[late.user]; g <= 0 || g > 0.01 {
		t.Errorf("late joiner earned %v", g)
	}

	// Throwaway accounts agreeing afterwards are worth almost nothing.
	lead := v(true, 50)
	votes = []vote{lead, v(true, 0), v(true, 0), v(true, 0), v(true, 0), v(true, 0)}
	if g := score(votes, in(statusConfirmed))[lead.user]; g > 0.08 {
		t.Errorf("sock puppets paid %v", g)
	}

	// Accounts on one network earn nothing from each other.
	x, y := v(true, 100), v(true, 100)
	x.net, y.net = "net-a", "net-a"
	if got := score([]vote{x, y}, in(statusConfirmed)); len(got) != 0 {
		t.Errorf("same network paid: %v", got)
	}

	// "Not slop" on a video nobody reported pays nothing; answering a
	// real report does, and the wrong first reporter pays extra.
	votes = []vote{v(false, 100), v(false, 100), v(false, 100)}
	if got := score(votes, in(statusRejected)); len(got) != 0 {
		t.Errorf("uncontested not-slop paid: %v", got)
	}
	reporter := v(true, 100)
	votes = []vote{reporter, v(false, 100), v(false, 100), v(false, 100)}
	got = score(votes, in(statusRejected))
	if got[votes[1].user] <= 0 || !near(got[reporter.user], -lossWrong*firstReportMul) {
		t.Errorf("contested not-slop: %v", got)
	}

	// An overturned listing costs its reporters double.
	votes = []vote{v(true, 100), v(true, 100)}
	sc := in(statusRejected)
	sc.byAdmin, sc.overturned = true, true
	got = score(votes, sc)
	if !near(got[votes[0].user], -lossWrong*overturnMul*firstReportMul) || !near(got[votes[1].user], -lossWrong*overturnMul) {
		t.Errorf("overturned: %v", got)
	}

	// Each further video from one channel pays less; none once it is listed.
	votes = []vote{v(true, 100), v(true, 100)}
	base := score(votes, in(statusConfirmed))[votes[1].user]
	sc = in(statusConfirmed)
	sc.chanPrior[votes[1].user] = 1
	if g := score(votes, sc)[votes[1].user]; !near(g, base/2) {
		t.Errorf("second video of a channel paid %v, first paid %v", g, base)
	}
	votes[1].chanListed = true
	if g := score(votes, in(statusConfirmed))[votes[1].user]; g != 0 {
		t.Errorf("video of a listed channel paid %v", g)
	}
}

// Several accounts behind one network count once towards a decision.
func TestOneNetworkOneVote(t *testing.T) {
	a, b := vote{user: "a", slop: true, rep: 100, types: 1, level: 2, net: "n1"}, vote{user: "b", slop: true, rep: 100, types: 1, level: 2, net: "n1"}
	if o := decide([]vote{a, b}, 0); o.status != statusPending || o.nYes != 1 {
		t.Errorf("one network settled a video: %+v", o)
	}
	b.net = "n2"
	if o := decide([]vote{a, b}, 0); o.status != statusConfirmed {
		t.Errorf("two networks did not settle: %+v", o)
	}
}

// Reputation keeps following later agreement, and a late vote on a decided
// video earns nothing.
func TestFollowersPayEarlierVoters(t *testing.T) {
	s := newTestServer(t)
	a, b, c := mkUser(t, s, 60), mkUser(t, s, 60), mkUser(t, s, 60)
	assess(t, s, a, vid(1), "", true)
	assess(t, s, b, vid(1), "", true)
	ra, rb := rep(t, s, a), rep(t, s, b)
	if vs := assess(t, s, c, vid(1), "", true); vs.Status != "confirmed" {
		t.Fatalf("status %s", vs.Status)
	}
	if rep(t, s, a) <= ra || rep(t, s, b) <= rb {
		t.Errorf("earlier voters did not gain from a follower: %v→%v, %v→%v", ra, rep(t, s, a), rb, rep(t, s, b))
	}
	if got := rep(t, s, c); got != 60 {
		t.Errorf("late voter on a decided video has %v", got)
	}
	// Counts on undecided videos are hidden from people who have not voted.
	assess(t, s, a, vid(2), "", true)
	if vs, _ := s.VideoState(context.Background(), vid(2), b); !vs.CountsHidden || vs.NYes != 0 {
		t.Errorf("pending counts visible to a non-voter: %+v", vs)
	}
	if vs, _ := s.VideoState(context.Background(), vid(2), a); vs.CountsHidden || vs.NYes != 1 {
		t.Errorf("pending counts hidden from the voter: %+v", vs)
	}
}

func TestDailyGainCap(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	u := mkUser(t, s, 10)
	apply := func(video string, target float64) {
		tx, err := s.db.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.applyScores(ctx, tx, video, map[string]float64{u.ID: target}); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	apply("v1", 2)
	apply("v2", 2)
	if got := rep(t, s, u); !near(got, 10+dailyGainCap) {
		t.Fatalf("after two gains of 2: %v", got)
	}
	// Losses are never capped.
	apply("v3", -1.5)
	if got := rep(t, s, u); !near(got, 10+dailyGainCap-1.5) {
		t.Fatalf("after a loss: %v", got)
	}
	// The held-back part arrives when the video is rescored another day.
	real := s.now
	s.now = func() time.Time { return real().Add(25 * time.Hour) }
	defer func() { s.now = real }()
	apply("v2", 2)
	if got := rep(t, s, u); !near(got, 10+4-1.5) {
		t.Fatalf("next day: %v", got)
	}
	// A video that stops paying takes back only what it paid.
	apply("v1", 0)
	tx, _ := s.db.Begin(ctx)
	if err := s.applyScores(ctx, tx, "v2", map[string]float64{}); err != nil {
		t.Fatal(err)
	}
	_ = tx.Commit(ctx)
	if got := rep(t, s, u); !near(got, 10-1.5) {
		t.Fatalf("after reversal: %v", got)
	}
}

// A burst of reports from new accounts puts the channel on hold: even
// established users cannot get it listed until an administrator releases it.
func TestBrigadeHold(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	held := func(uc string) bool {
		var h bool
		if err := s.db.QueryRow(ctx, `SELECT COALESCE((SELECT held FROM channels WHERE uc = $1), false)`, uc).Scan(&h); err != nil {
			t.Fatal(err)
		}
		return h
	}
	for i := 0; i < brigadeReporters; i++ {
		if i == brigadeReporters-1 && held(chn(9)) {
			t.Fatal("held too early")
		}
		assess(t, s, mkNewUser(t, s, 0), vid(i%2), chn(9), true)
	}
	if !held(chn(9)) {
		t.Fatal("no hold after a burst from new accounts")
	}
	a, b := mkUser(t, s, 90), mkUser(t, s, 90)
	assess(t, s, a, vid(0), chn(9), true)
	if vs := assess(t, s, b, vid(0), chn(9), true); vs.Status != "pending" {
		t.Fatalf("listed while on hold: %s", vs.Status)
	}
	if rep(t, s, a) != 90 {
		t.Error("reputation paid while on hold")
	}
	if err := s.releaseHold(ctx, "channel", chn(9)); err != nil {
		t.Fatal(err)
	}
	if vs, _ := s.VideoState(ctx, vid(0), a); vs.Status != "confirmed" || held(chn(9)) {
		t.Fatalf("after release: %+v", vs)
	}
	// More new accounts do not start another hold straight away.
	assess(t, s, mkNewUser(t, s, 0), vid(1), chn(9), true)
	if held(chn(9)) {
		t.Fatal("held again right after release")
	}

	// The same burst from long-standing accounts is not a brigade.
	for i := 0; i < brigadeReporters+2; i++ {
		assess(t, s, mkUser(t, s, 20), vid(50+i), chn(10), true)
	}
	if held(chn(10)) {
		t.Fatal("established accounts triggered a hold")
	}
}

// Agreeing with the same partner over and over must pay less each time,
// while agreeing with new people keeps paying.
func TestPairDiversity(t *testing.T) {
	s := newTestServer(t)
	a, b := mkUser(t, s, 30), mkUser(t, s, 30)
	var gains []float64
	for i := 1; i <= 4; i++ {
		before := rep(t, s, b)
		assess(t, s, a, vid(i), "", true)
		assess(t, s, b, vid(i), "", true)
		gains = append(gains, rep(t, s, b)-before)
	}
	for i := 1; i < len(gains); i++ {
		if gains[i] >= gains[i-1] {
			t.Fatalf("repeat agreement did not shrink: %v", gains)
		}
	}
	// A fresh partner pays b more than the worn-out one now does.
	c := mkUser(t, s, 30)
	before := rep(t, s, b)
	assess(t, s, c, vid(10), "", true)
	assess(t, s, b, vid(10), "", true)
	if g := rep(t, s, b) - before; g <= gains[len(gains)-1] {
		t.Errorf("new partner paid %v, worn-out partner paid %v", g, gains[len(gains)-1])
	}
}

// A ring of brand-new accounts agreeing with each other must not be able
// to list a video or build reputation.
func TestSybilRing(t *testing.T) {
	s := newTestServer(t)
	var ring []*User
	for i := 0; i < 8; i++ {
		ring = append(ring, mkUser(t, s, 0))
	}
	for n := 1; n <= 5; n++ {
		for _, u := range ring {
			if vs := assess(t, s, u, vid(n), chn(1), true); vs.Status != "pending" {
				t.Fatalf("ring settled video %d: %s", n, vs.Status)
			}
		}
	}
	for _, u := range ring {
		if r := rep(t, s, u); r != 0 {
			t.Fatalf("ring member gained reputation: %v", r)
		}
	}
}

func TestDailyLimit(t *testing.T) {
	s := newTestServer(t)
	u := mkUser(t, s, 0)
	limit := dailyReportLimit(0)
	for i := 0; i < limit; i++ {
		assess(t, s, u, vid(i), "", true)
	}
	_, err := s.Assess(context.Background(), u, assessmentInput{Video: vid(999), Slop: true, Types: []string{"voice"}, Level: 1})
	if ae, ok := err.(*apiError); !ok || ae.Code != "daily_limit" {
		t.Fatalf("expected daily_limit, got %v", err)
	}
	// Editing an existing judgement is still allowed.
	if _, err := s.Assess(context.Background(), u, assessmentInput{Video: vid(0), Slop: false}); err != nil {
		t.Fatalf("edit blocked: %v", err)
	}
}

func TestChannelFlagging(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	a, b, c := mkUser(t, s, 60), mkUser(t, s, 60), mkUser(t, s, 60)
	flagged := func() bool {
		var f bool
		if err := s.db.QueryRow(ctx, `SELECT COALESCE((SELECT flagged FROM channels WHERE uc = $1), false)`, chn(7)).Scan(&f); err != nil {
			t.Fatal(err)
		}
		return f
	}
	for n := 1; n <= 3; n++ {
		assess(t, s, a, vid(n), chn(7), true)
		assess(t, s, b, vid(n), chn(7), true)
		if n < 3 && flagged() {
			t.Fatalf("flagged after %d videos", n)
		}
	}
	// Three confirmed videos but only two distinct reporters: not yet.
	if flagged() {
		t.Fatal("flagged with only two reporters")
	}
	assess(t, s, c, vid(3), chn(7), true)
	if !flagged() {
		t.Fatal("not flagged after three videos from three reporters")
	}

	// It shows up in the snapshot and the delta.
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v1/snapshot", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	s.Routes().ServeHTTP(rr, req)
	zr, err := gzip.NewReader(rr.Body)
	if err != nil {
		t.Fatal(err)
	}
	var snap struct {
		Seq      int64
		Videos   [][]any
		Channels [][]any
	}
	if err := json.NewDecoder(zr).Decode(&snap); err != nil {
		t.Fatal(err)
	}
	if len(snap.Videos) != 3 || len(snap.Channels) != 1 || snap.Channels[0][0] != chn(7) || snap.Seq == 0 {
		t.Fatalf("snapshot: %+v", snap)
	}
	etag := rr.Header().Get("ETag")
	rr = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/v1/snapshot", nil)
	req.Header.Set("If-None-Match", etag)
	s.Routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusNotModified {
		t.Errorf("conditional snapshot = %d", rr.Code)
	}

	// An admin ruling can clear the channel, and the delta reports that.
	if err := s.db.QueryRow(ctx, `UPDATE channels SET override = -1 WHERE uc = $1 RETURNING true`, chn(7)).Scan(new(bool)); err != nil {
		t.Fatal(err)
	}
	tx, _ := s.db.Begin(ctx)
	if err := s.recomputeChannel(ctx, tx, chn(7)); err != nil {
		t.Fatal(err)
	}
	_ = tx.Commit(ctx)
	rr = httptest.NewRecorder()
	s.Routes().ServeHTTP(rr, httptest.NewRequest("GET", "/v1/delta?since="+strconv.FormatInt(snap.Seq, 10), nil))
	var delta struct {
		Seq      int64
		Channels [][]any
	}
	if err := json.NewDecoder(rr.Body).Decode(&delta); err != nil {
		t.Fatal(err)
	}
	if len(delta.Channels) != 1 || delta.Channels[0][1].(float64) != 0 || delta.Seq <= snap.Seq {
		t.Fatalf("delta: %+v", delta)
	}
	rr = httptest.NewRecorder()
	s.Routes().ServeHTTP(rr, httptest.NewRequest("GET", "/v1/delta?since=999999", nil))
	if rr.Code != http.StatusGone {
		t.Errorf("delta from the future = %d", rr.Code)
	}
}

func TestChannelMappingQuorum(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	claim := func(source string, w float32, handle string) {
		t.Helper()
		if _, err := s.SubmitClaims(ctx, source, w, []channelClaim{{UC: chn(1), Handle: handle, Name: "Some Channel"}}); err != nil {
			t.Fatal(err)
		}
	}
	served := func() (string, bool) {
		t.Helper()
		m, err := s.LookupChannels(ctx, []string{chn(1), "not-an-id"})
		if err != nil {
			t.Fatal(err)
		}
		ci, ok := m[chn(1)]
		return ci.Handle, ok
	}

	claim("n:a", claimWeightNet, "@Real")
	claim("n:b", claimWeightNet, "@real")
	claim("n:a", claimWeightNet, "@REAL") // the same network again does not add up
	if _, ok := served(); ok {
		t.Fatal("served after two sources")
	}
	claim("n:c", claimWeightNet, "@real")
	if h, ok := served(); !ok || h != "@real" {
		t.Fatalf("not served after three sources: %q %v", h, ok)
	}

	// Two reputable accounts are enough on their own.
	if _, err := s.SubmitClaims(ctx, "u:1", claimWeightAcct, []channelClaim{{UC: chn(2), Handle: "@two"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SubmitClaims(ctx, "u:2", claimWeightAcct, []channelClaim{{UC: chn(2), Handle: "@two"}}); err != nil {
		t.Fatal(err)
	}
	if m, _ := s.LookupChannels(ctx, []string{chn(2)}); m[chn(2)].Handle != "@two" {
		t.Fatalf("two accounts did not confirm: %+v", m)
	}

	// One dissenter changes nothing; enough independent dissent withdraws
	// the mapping, and a clear new winner replaces it.
	claim("n:x", claimWeightNet, "@evil")
	if h, _ := served(); h != "@real" {
		t.Fatalf("one dissenter changed the mapping to %q", h)
	}
	claim("n:y", claimWeightNet, "@renamed")
	claim("n:z", claimWeightNet, "@renamed")
	claim("n:w", claimWeightNet, "@renamed")
	if _, ok := served(); ok {
		t.Fatal("still served while sources disagree")
	}
	claim("n:a", claimWeightNet, "@renamed")
	claim("n:b", claimWeightNet, "@renamed")
	claim("n:c", claimWeightNet, "@renamed")
	if h, ok := served(); !ok || h != "@renamed" {
		t.Fatalf("rename not picked up: %q %v", h, ok)
	}

	// Garbage is ignored.
	n, err := s.SubmitClaims(ctx, "n:a", 1, []channelClaim{{UC: "nope", Handle: "@x"}, {UC: chn(3), Handle: "@has/slash"}})
	if err != nil || n != 0 {
		t.Fatalf("invalid claims accepted: %d %v", n, err)
	}
}

func TestClaimSource(t *testing.T) {
	if src, w := claimSource(nil, "net1"); src != "n:net1" || w != claimWeightNet {
		t.Errorf("anonymous: %s %v", src, w)
	}
	if src, _ := claimSource(&User{ID: "u1", Reputation: 1}, "net1"); src != "n:net1" {
		t.Errorf("a new account must count as its network, got %s", src)
	}
	if src, w := claimSource(&User{ID: "u1", Reputation: 50}, "net1"); src != "u:u1" || w != claimWeightAcct {
		t.Errorf("reputable account: %s %v", src, w)
	}
	if src, _ := claimSource(&User{ID: "u1", Reputation: 50, Banned: true}, "net1"); src != "n:net1" {
		t.Errorf("banned account counted as an account: %s", src)
	}
}

func TestClientAddr(t *testing.T) {
	s := &Server{}
	p, _ := ConfigFromEnvForTest("10.0.0.0/8,192.0.2.1")
	s.cfg.TrustedProxies = p
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "10.1.2.3:5555"
	req.Header.Set("X-Forwarded-For", "6.6.6.6, 203.0.113.9, 192.0.2.1")
	if got := s.clientAddr(req).String(); got != "203.0.113.9" {
		t.Errorf("behind proxies: %s", got)
	}
	// A direct caller cannot spoof the header.
	req.RemoteAddr = "198.51.100.7:1"
	if got := s.clientAddr(req).String(); got != "198.51.100.7" {
		t.Errorf("direct: %s", got)
	}
}

func TestPow(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	rr := httptest.NewRecorder()
	s.Routes().ServeHTTP(rr, httptest.NewRequest("GET", "/auth/pow", nil))
	var ch powChallenge
	if err := json.NewDecoder(rr.Body).Decode(&ch); err != nil || ch.Challenge == "" {
		t.Fatalf("pow: %v %+v", err, ch)
	}
	nonce := ""
	for i := 0; ; i++ {
		sum := sha256.Sum256([]byte(ch.Challenge + ":" + strconv.Itoa(i)))
		if leadingZeroBits(sum[:]) >= ch.Bits {
			nonce = strconv.Itoa(i)
			break
		}
	}
	if err := s.checkPow(ctx, ch.Challenge, nonce); err != nil {
		t.Fatalf("valid proof rejected: %v", err)
	}
	if err := s.checkPow(ctx, ch.Challenge, nonce); err == nil {
		t.Fatal("proof accepted twice")
	}
	if err := s.checkPow(ctx, "made-up", "1"); err == nil {
		t.Fatal("unknown challenge accepted")
	}
}

func TestOAuthFlow(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	h := s.Routes()
	u := mkUser(t, s, 10)

	// Sign the user in to the web session directly.
	rr := httptest.NewRecorder()
	if err := s.startSession(ctx, rr, u.ID); err != nil {
		t.Fatal(err)
	}
	cookie := rr.Result().Cookies()[0]

	verifier := "v-" + strings.Repeat("x", 50)
	sum := sha256.Sum256([]byte(verifier))
	q := url.Values{
		"client_id": {oauthClientID}, "response_type": {"code"}, "redirect_uri": {"https://ext.example/cb"},
		"code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "code_challenge_method": {"S256"}, "state": {"st8"},
	}

	// Unregistered redirect targets are refused outright.
	bad := url.Values{}
	for k, v := range q {
		bad[k] = v
	}
	bad.Set("redirect_uri", "https://evil.example/cb")
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/oauth/authorize?"+bad.Encode(), nil))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("evil redirect: %d", rr.Code)
	}

	// Without a session the user is sent to sign in.
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/oauth/authorize?"+q.Encode(), nil))
	if rr.Code != http.StatusSeeOther || !strings.HasPrefix(rr.Header().Get("Location"), "/login?next=") {
		t.Fatalf("anonymous authorize: %d %s", rr.Code, rr.Header().Get("Location"))
	}

	// A cross-site approval is refused; a same-origin one issues a code.
	post := func(origin string) *httptest.ResponseRecorder {
		form := url.Values{"decision": {"allow"}}
		for k, v := range q {
			form[k] = v
		}
		req := httptest.NewRequest("POST", "/oauth/authorize", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", origin)
		req.AddCookie(cookie)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}
	if rr := post("https://evil.example"); rr.Code != http.StatusForbidden {
		t.Fatalf("cross-site approval: %d", rr.Code)
	}
	rr = post("http://localhost:8080")
	loc, _ := url.Parse(rr.Header().Get("Location"))
	code := loc.Query().Get("code")
	if rr.Code != http.StatusSeeOther || code == "" || loc.Query().Get("state") != "st8" || loc.Host != "ext.example" {
		t.Fatalf("approval: %d %s", rr.Code, rr.Header().Get("Location"))
	}

	token := func(form url.Values) (int, tokenResponse) {
		form.Set("client_id", oauthClientID)
		req := httptest.NewRequest("POST", "/oauth/token", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		var tr tokenResponse
		_ = json.NewDecoder(rr.Body).Decode(&tr)
		return rr.Code, tr
	}
	exchange := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {"https://ext.example/cb"}}

	// Wrong verifier fails and burns the code.
	exchange.Set("code_verifier", "wrong")
	if st, _ := token(exchange); st != http.StatusBadRequest {
		t.Fatalf("wrong verifier: %d", st)
	}
	exchange.Set("code_verifier", verifier)
	if st, _ := token(exchange); st != http.StatusBadRequest {
		t.Fatalf("code reuse after failure: %d", st)
	}

	// A fresh code with the right verifier works.
	rr = post("http://localhost:8080")
	loc, _ = url.Parse(rr.Header().Get("Location"))
	exchange.Set("code", loc.Query().Get("code"))
	st, tok := token(exchange)
	if st != http.StatusOK || tok.AccessToken == "" || tok.RefreshToken == "" {
		t.Fatalf("exchange: %d %+v", st, tok)
	}

	me := func(access string) int {
		req := httptest.NewRequest("GET", "/v1/me", nil)
		req.Header.Set("Authorization", "Bearer "+access)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr.Code
	}
	if me(tok.AccessToken) != http.StatusOK || me("nope") != http.StatusUnauthorized {
		t.Fatal("bearer authentication is wrong")
	}

	// Refresh rotates; replaying the old refresh token ends the sign-in.
	st, tok2 := token(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tok.RefreshToken}})
	if st != http.StatusOK || tok2.RefreshToken == tok.RefreshToken {
		t.Fatalf("refresh: %d", st)
	}
	if st, _ := token(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tok.RefreshToken}}); st != http.StatusBadRequest {
		t.Fatalf("refresh replay: %d", st)
	}
	if me(tok2.AccessToken) != http.StatusUnauthorized {
		t.Fatal("tokens survived a refresh-token replay")
	}

	// Reporting through the API with a new sign-in.
	rr = post("http://localhost:8080")
	loc, _ = url.Parse(rr.Header().Get("Location"))
	exchange.Set("code", loc.Query().Get("code"))
	_, tok = token(exchange)
	req := httptest.NewRequest("POST", "/v1/assessments", strings.NewReader(`{"video":"abcdefghijk","uc":"`+chn(5)+`","slop":true,"types":["music"],"level":3}`))
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	var vs videoState
	_ = json.NewDecoder(rr.Body).Decode(&vs)
	if rr.Code != http.StatusOK || vs.Status != "pending" || vs.Mine == nil || !vs.Mine.Slop {
		t.Fatalf("report via API: %d %+v", rr.Code, vs)
	}
	// Anonymous callers cannot report.
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("POST", "/v1/assessments", strings.NewReader(`{"video":"abcdefghijk","slop":false}`)))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous report: %d", rr.Code)
	}
}

func TestAdminClaimAndOverride(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	h := s.Routes()
	admin, other := mkUser(t, s, 0), mkUser(t, s, 0)
	session := func(u *User) *http.Cookie {
		rr := httptest.NewRecorder()
		if err := s.startSession(ctx, rr, u.ID); err != nil {
			t.Fatal(err)
		}
		return rr.Result().Cookies()[0]
	}
	get := func(path string, c *http.Cookie) int {
		req := httptest.NewRequest("GET", path, nil)
		req.AddCookie(c)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr.Code
	}
	ac, oc := session(admin), session(other)
	if get("/admin", ac) != http.StatusNotFound {
		t.Fatal("admin page visible before claiming")
	}
	if get("/admin/claim?token=wrong", ac) != http.StatusNotFound {
		t.Fatal("wrong token accepted")
	}
	if get("/admin/claim?token=admin-secret", ac) != http.StatusSeeOther {
		t.Fatal("claim failed")
	}
	if get("/admin/claim?token=admin-secret", oc) != http.StatusConflict {
		t.Fatal("second admin claim allowed")
	}
	if get("/admin", ac) != http.StatusOK || get("/admin", oc) != http.StatusNotFound {
		t.Fatal("admin page access is wrong")
	}

	// A ruling settles a video on the spot and pays those who agreed.
	assess(t, s, other, vid(1), chn(1), true)
	form := url.Values{"id": {vid(1)}, "action": {"confirm"}}
	req := httptest.NewRequest("POST", "/admin/video", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://localhost:8080")
	req.AddCookie(ac)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("override: %d %s", rr.Code, rr.Body.String())
	}
	vs, _ := s.VideoState(ctx, vid(1), nil)
	if vs.Status != "confirmed" {
		t.Fatalf("after ruling: %s", vs.Status)
	}
	if rep(t, s, other) <= 0 {
		t.Error("agreeing with a ruling earned nothing")
	}
}

// ConfigFromEnvForTest parses a TRUSTED_PROXIES value.
func ConfigFromEnvForTest(v string) ([]netip.Prefix, error) {
	os.Setenv("TRUSTED_PROXIES", v)
	os.Setenv("DATABASE_URL", "x")
	defer os.Unsetenv("TRUSTED_PROXIES")
	defer os.Unsetenv("DATABASE_URL")
	c, err := ConfigFromEnv()
	return c.TrustedProxies, err
}
