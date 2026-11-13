package server

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The read side: clients download one snapshot of everything confirmed,
// then ask for the changes since the sequence number they hold.

const (
	snapshotTTL = 15 * time.Second // how stale the cached snapshot may be
	deltaLimit  = 5000
)

type snapshotCache struct {
	mu      sync.Mutex
	checked time.Time
	seq     int64
	body    []byte // gzip-compressed JSON
	etag    string
	built   bool
}

func (s *Server) currentSeq(ctx context.Context) (int64, error) {
	var seq int64
	err := s.db.QueryRow(ctx,
		`SELECT GREATEST(COALESCE((SELECT max(seq) FROM videos WHERE seq > 0), 0),
		                 COALESCE((SELECT max(seq) FROM channels WHERE seq > 0), 0))`).Scan(&seq)
	return seq, err
}

func (s *Server) snapshot(ctx context.Context) ([]byte, string, error) {
	c := &s.snap
	c.mu.Lock()
	defer c.mu.Unlock()
	now := s.now()
	if c.built && now.Sub(c.checked) < snapshotTTL {
		return c.body, c.etag, nil
	}
	seq, err := s.currentSeq(ctx)
	if err != nil {
		return nil, "", err
	}
	if c.built && seq == c.seq {
		c.checked = now
		return c.body, c.etag, nil
	}

	videos := [][]any{}
	rows, err := s.db.Query(ctx, `SELECT id, types, level FROM videos WHERE status = 1 AND seq <= $1 ORDER BY id`, seq)
	if err != nil {
		return nil, "", err
	}
	for rows.Next() {
		var id string
		var types, level int
		if err := rows.Scan(&id, &types, &level); err != nil {
			rows.Close()
			return nil, "", err
		}
		videos = append(videos, []any{id, types, level})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, "", err
	}

	channels := [][]any{}
	rows, err = s.db.Query(ctx, `SELECT uc, COALESCE(handle, '') FROM channels WHERE flagged AND seq <= $1 ORDER BY uc`, seq)
	if err != nil {
		return nil, "", err
	}
	for rows.Next() {
		var uc, handle string
		if err := rows.Scan(&uc, &handle); err != nil {
			rows.Close()
			return nil, "", err
		}
		channels = append(channels, []any{uc, handle})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, "", err
	}

	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err := json.NewEncoder(zw).Encode(map[string]any{"seq": seq, "videos": videos, "channels": channels}); err != nil {
		return nil, "", err
	}
	if err := zw.Close(); err != nil {
		return nil, "", err
	}
	c.body, c.seq, c.checked, c.built = buf.Bytes(), seq, now, true
	c.etag = `"s` + strconv.FormatInt(seq, 10) + `"`
	return c.body, c.etag, nil
}

func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) error {
	body, etag, err := s.snapshot(r.Context())
	if err != nil {
		return err
	}
	h := w.Header()
	h.Set("ETag", etag)
	h.Set("Cache-Control", "public, max-age=300")
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Vary", "Accept-Encoding")
	if match := r.Header.Get("If-None-Match"); match != "" && strings.Contains(match, etag) {
		w.WriteHeader(http.StatusNotModified)
		return nil
	}
	h.Set("Content-Type", "application/json; charset=utf-8")
	if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		h.Set("Content-Encoding", "gzip")
		h.Set("Content-Length", strconv.Itoa(len(body)))
		_, _ = w.Write(body)
		return nil
	}
	zr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer zr.Close()
	var raw bytes.Buffer
	if _, err := raw.ReadFrom(zr); err != nil {
		return err
	}
	_, _ = w.Write(raw.Bytes())
	return nil
}

// handleDelta returns what changed after ?since. A removed entry comes back
// with a 0 flag. Clients that are too far behind are told to resync.
func (s *Server) handleDelta(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	since, err := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	if err != nil || since < 0 {
		return errf(http.StatusBadRequest, "bad_request", "since must be a sequence number")
	}
	seq, err := s.currentSeq(ctx)
	if err != nil {
		return err
	}
	resync := func() error {
		writeJSON(w, http.StatusGone, &apiError{Code: "resync", Msg: "download /v1/snapshot again"})
		return nil
	}
	if since > seq {
		return resync()
	}

	videos := [][]any{}
	channels := [][]any{}
	if since < seq {
		rows, err := s.db.Query(ctx,
			`SELECT id, status, types, level FROM videos WHERE seq > $1 AND seq <= $2 LIMIT $3`,
			since, seq, deltaLimit+1)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id string
			var status, types, level int
			if err := rows.Scan(&id, &status, &types, &level); err != nil {
				rows.Close()
				return err
			}
			on := 0
			if status == statusConfirmed {
				on = 1
			}
			videos = append(videos, []any{id, on, types, level})
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		rows, err = s.db.Query(ctx,
			`SELECT uc, flagged, COALESCE(handle, '') FROM channels WHERE seq > $1 AND seq <= $2 LIMIT $3`,
			since, seq, deltaLimit+1)
		if err != nil {
			return err
		}
		for rows.Next() {
			var uc, handle string
			var flagged bool
			if err := rows.Scan(&uc, &flagged, &handle); err != nil {
				rows.Close()
				return err
			}
			on := 0
			if flagged {
				on = 1
			}
			channels = append(channels, []any{uc, on, handle})
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if len(videos) > deltaLimit || len(channels) > deltaLimit {
			return resync()
		}
	}
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=60")
	return json.NewEncoder(w).Encode(map[string]any{"seq": seq, "videos": videos, "channels": channels})
}
