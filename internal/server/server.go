// Package server implements the aislop service: a community database of
// AI-slop videos and channels with reputation-weighted consensus.
package server

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed schema.sql
var schemaSQL string

// Config comes from the environment; see README.
type Config struct {
	DatabaseURL    string
	Listen         string
	Origin         string // public origin, e.g. https://aislop.tikaydaes.dev
	TrustedProxies []netip.Prefix
	RedirectURIs   []string // exact OAuth redirect URIs the extension may use
	AdminToken     string   // lets the first signed-in user claim the admin role
	PowBits        int
}

func ConfigFromEnv() (Config, error) {
	c := Config{
		DatabaseURL: os.Getenv("DATABASE_URL"),
		Listen:      envOr("LISTEN", ":8080"),
		Origin:      strings.TrimRight(envOr("PUBLIC_ORIGIN", "http://localhost:8080"), "/"),
		AdminToken:  os.Getenv("ADMIN_TOKEN"),
		PowBits:     18,
	}
	if c.DatabaseURL == "" {
		return c, errors.New("DATABASE_URL is required")
	}
	if v := os.Getenv("POW_BITS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > 28 {
			return c, errors.New("POW_BITS must be 0..28")
		}
		c.PowBits = n
	}
	for _, s := range splitList(os.Getenv("TRUSTED_PROXIES")) {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			a, err2 := netip.ParseAddr(s)
			if err2 != nil {
				return c, fmt.Errorf("TRUSTED_PROXIES: %q is not an address or prefix", s)
			}
			p = netip.PrefixFrom(a, a.BitLen())
		}
		c.TrustedProxies = append(c.TrustedProxies, p)
	}
	c.RedirectURIs = splitList(os.Getenv("OAUTH_REDIRECT_URIS"))
	if _, err := url.Parse(c.Origin); err != nil {
		return c, fmt.Errorf("PUBLIC_ORIGIN: %w", err)
	}
	return c, nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' }) {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

type Server struct {
	cfg   Config
	db    *pgxpool.Pool
	wa    *webauthn.WebAuthn
	log   *slog.Logger
	snap  snapshotCache
	limit *limiter
	now   func() time.Time // replaceable in tests

	saltMu  sync.Mutex
	salt    []byte
	saltEra int64
}

func New(ctx context.Context, cfg Config, log *slog.Logger) (*Server, error) {
	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, err
	}
	if _, err := pool.Exec(ctx, schemaSQL); err != nil {
		pool.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	u, _ := url.Parse(cfg.Origin)
	wa, err := webauthn.New(&webauthn.Config{
		RPID:          u.Hostname(),
		RPDisplayName: "AI Slop Reports",
		RPOrigins:     []string{cfg.Origin},
	})
	if err != nil {
		pool.Close()
		return nil, err
	}
	return &Server{cfg: cfg, db: pool, wa: wa, log: log, limit: newLimiter(), now: time.Now}, nil
}

func (s *Server) Close() { s.db.Close() }

// ---------------------------------------------------------------- helpers

func randToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func hashHex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

type apiError struct {
	Status int    `json:"-"`
	Code   string `json:"error"`
	Msg    string `json:"message,omitempty"`
}

func (e *apiError) Error() string { return e.Code + ": " + e.Msg }

func errf(status int, code, format string, a ...any) *apiError {
	return &apiError{Status: status, Code: code, Msg: fmt.Sprintf(format, a...)}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) writeErr(w http.ResponseWriter, r *http.Request, err error) {
	var ae *apiError
	if errors.As(err, &ae) {
		writeJSON(w, ae.Status, ae)
		return
	}
	s.log.Error("request failed", "path", r.URL.Path, "err", err)
	writeJSON(w, http.StatusInternalServerError, &apiError{Code: "internal"})
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 256<<10)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(v); err != nil {
		return errf(http.StatusBadRequest, "bad_request", "invalid JSON body")
	}
	return nil
}

// handler adapts a function returning an error to http.HandlerFunc.
func (s *Server) handler(fn func(w http.ResponseWriter, r *http.Request) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := fn(w, r); err != nil {
			s.writeErr(w, r, err)
		}
	}
}

// ------------------------------------------------- client network identity

// clientAddr returns the caller's address, trusting X-Forwarded-For only
// for hops inside the configured proxy ranges.
func (s *Server) clientAddr(r *http.Request) netip.Addr {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}
	}
	addr = addr.Unmap()
	if !s.trusted(addr) {
		return addr
	}
	hops := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			break
		}
		addr = a.Unmap()
		if !s.trusted(addr) {
			break
		}
	}
	return addr
}

func (s *Server) trusted(a netip.Addr) bool {
	for _, p := range s.cfg.TrustedProxies {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

const saltPeriod = 30 * 24 * time.Hour

// netKey maps the caller's network (/24 for IPv4, /48 for IPv6) to an
// opaque key. The salt rotates every 30 days and old salts are deleted, so
// keys cannot be linked across periods or turned back into addresses.
func (s *Server) netKey(ctx context.Context, r *http.Request) string {
	addr := s.clientAddr(r)
	if !addr.IsValid() {
		return "unknown"
	}
	bits := 24
	if addr.Is6() {
		bits = 48
	}
	prefix, _ := addr.Prefix(bits)
	salt, err := s.currentSalt(ctx)
	if err != nil {
		s.log.Error("salt", "err", err)
		return "unknown"
	}
	m := hmac.New(sha256.New, salt)
	m.Write([]byte(prefix.String()))
	return hex.EncodeToString(m.Sum(nil)[:10])
}

func (s *Server) currentSalt(ctx context.Context) ([]byte, error) {
	era := s.now().Unix() / int64(saltPeriod/time.Second)
	s.saltMu.Lock()
	defer s.saltMu.Unlock()
	if s.salt != nil && s.saltEra == era {
		return s.salt, nil
	}
	key := "salt:" + strconv.FormatInt(era, 10)
	var v string
	err := s.db.QueryRow(ctx,
		`INSERT INTO meta (k, v) VALUES ($1, $2)
		 ON CONFLICT (k) DO UPDATE SET v = meta.v RETURNING v`, key, randToken(32)).Scan(&v)
	if err != nil {
		return nil, err
	}
	_, _ = s.db.Exec(ctx, `DELETE FROM meta WHERE k LIKE 'salt:%' AND k <> $1`, key)
	s.salt, s.saltEra = []byte(v), era
	return s.salt, nil
}

// ---------------------------------------------------------- rate limiting

// limiter is a fixed-window counter per key. It is deliberately simple:
// one process serves the whole site.
type limiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	n     int
	reset time.Time
}

func newLimiter() *limiter { return &limiter{buckets: map[string]*bucket{}} }

// allow consumes cost from key's budget of max per window.
func (l *limiter) allow(key string, cost, max int, window time.Duration, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.buckets[key]
	if b == nil || now.After(b.reset) {
		b = &bucket{reset: now.Add(window)}
		l.buckets[key] = b
	}
	if b.n+cost > max {
		return false
	}
	b.n += cost
	return true
}

func (l *limiter) sweep(now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for k, b := range l.buckets {
		if now.After(b.reset) {
			delete(l.buckets, k)
		}
	}
}

func (s *Server) rateLimit(ctx context.Context, r *http.Request, name string, cost, max int, window time.Duration) error {
	if !s.limit.allow(name+":"+s.netKey(ctx, r), cost, max, window, s.now()) {
		return errf(http.StatusTooManyRequests, "rate_limited", "too many requests, try again later")
	}
	return nil
}

// ------------------------------------------------------------ maintenance

// RunJobs runs periodic maintenance until ctx is cancelled.
func (s *Server) RunJobs(ctx context.Context) {
	minute := time.NewTicker(time.Minute)
	hour := time.NewTicker(time.Hour)
	defer minute.Stop()
	defer hour.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-minute.C:
			s.limit.sweep(s.now())
			if err := s.RecomputePending(ctx, 500); err != nil {
				s.log.Error("recompute pending", "err", err)
			}
		case <-hour.C:
			if err := s.Housekeeping(ctx); err != nil {
				s.log.Error("housekeeping", "err", err)
			}
		}
	}
}

// Housekeeping expires short-lived rows and applies slow reputation decay.
func (s *Server) Housekeeping(ctx context.Context) error {
	now := s.now()
	stmts := []string{
		`DELETE FROM ephemeral WHERE expires_at < $1`,
		`DELETE FROM tokens WHERE expires_at < $1`,
		`DELETE FROM channel_claims WHERE seen_at < $1 - interval '30 days'`,
		`UPDATE users SET net_key = '' WHERE net_key <> '' AND created_at < $1 - interval '30 days'`,
		`UPDATE contests SET net_key = '' WHERE net_key <> '' AND created_at < $1 - interval '30 days'`,
	}
	for _, q := range stmts {
		if _, err := s.db.Exec(ctx, q, now); err != nil {
			return err
		}
	}
	// Reputation fades about 1% a week without activity to keep it earned.
	_, err := s.db.Exec(ctx,
		`UPDATE users SET reputation = reputation * 0.99994 WHERE reputation > 0 AND NOT is_admin`)
	return err
}
