// Package cache holds the rendered cards in memory and mirrors them to disk.
//
// Requests only ever read from memory. The disk copy exists so a restart or a
// reboot serves the last good card immediately instead of a blank one while
// the first refresh runs.
package cache

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Snapshot is one rendered card plus everything /healthz reports about it.
type Snapshot struct {
	SVG        []byte
	ETag       string
	RenderedAt time.Time

	// Gzip is the same SVG, compressed once here rather than on every
	// request. An SVG card is mostly repeated markup, so this is roughly a
	// quarter of the bytes on the wire for no per-request CPU.
	Gzip []byte

	// Summary is the human-readable detail for /healthz: the numbers on the
	// card and any config warnings.
	Summary map[string]any
}

// Status is the health of one card.
type Status struct {
	Name            string         `json:"name"`
	Healthy         bool           `json:"healthy"`
	HasRender       bool           `json:"has_render"`
	RenderedAt      *time.Time     `json:"rendered_at,omitempty"`
	LastSuccessAt   *time.Time     `json:"last_success_at,omitempty"`
	LastAttemptAt   *time.Time     `json:"last_attempt_at,omitempty"`
	LastError       string         `json:"last_error,omitempty"`
	LastErrorAt     *time.Time     `json:"last_error_at,omitempty"`
	ConsecutiveFail int            `json:"consecutive_failures"`
	LoadedFromDisk  bool           `json:"loaded_from_disk"`
	ETag            string         `json:"etag,omitempty"`
	Summary         map[string]any `json:"summary,omitempty"`
}

type entry struct {
	snap            *Snapshot
	lastSuccess     time.Time
	lastAttempt     time.Time
	lastError       string
	lastErrorAt     time.Time
	consecutiveFail int
	loadedFromDisk  bool
}

// Store keeps every card.
type Store struct {
	dir string

	mu      sync.RWMutex
	entries map[string]*entry
}

// New creates a store backed by dir. The directory is created if missing.
func New(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("create state dir %s: %w", dir, err)
	}
	return &Store{dir: dir, entries: map[string]*entry{}}, nil
}

func (s *Store) get(name string) *entry {
	e := s.entries[name]
	if e == nil {
		e = &entry{}
		s.entries[name] = e
	}
	return e
}

// Snapshot returns the current render, or nil if there has never been one.
func (s *Store) Snapshot(name string) *Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if e := s.entries[name]; e != nil {
		return e.snap
	}
	return nil
}

// Publish stores a fresh render and mirrors it to disk.
//
// Only call this with a complete render. A failed refresh must leave the
// previous snapshot in place; that is what keeps a broken API from putting an
// empty card in a README.
func (s *Store) Publish(name string, svg []byte, summary map[string]any, now time.Time) error {
	sum := sha256.Sum256(svg)
	snap := &Snapshot{
		SVG:        svg,
		ETag:       `"` + hex.EncodeToString(sum[:16]) + `"`,
		RenderedAt: now,
		Summary:    summary,
		Gzip:       gzipBytes(svg),
	}

	s.mu.Lock()
	e := s.get(name)
	e.snap = snap
	e.lastSuccess = now
	e.lastAttempt = now
	e.lastError = ""
	e.consecutiveFail = 0
	e.loadedFromDisk = false
	s.mu.Unlock()

	return s.persist(name, snap)
}

// Fail records a refresh that did not produce a render. The previous
// snapshot keeps serving.
func (s *Store) Fail(name string, err error, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.get(name)
	e.lastAttempt = now
	e.lastError = err.Error()
	e.lastErrorAt = now
	e.consecutiveFail++
}

// Status reports the health of one card.
func (s *Store) Status(name string) Status {
	s.mu.RLock()
	defer s.mu.RUnlock()

	st := Status{Name: name}
	e := s.entries[name]
	if e == nil {
		return st
	}
	if e.snap != nil {
		st.HasRender = true
		st.ETag = e.snap.ETag
		st.Summary = e.snap.Summary
		t := e.snap.RenderedAt
		st.RenderedAt = &t
	}
	if !e.lastSuccess.IsZero() {
		t := e.lastSuccess
		st.LastSuccessAt = &t
	}
	if !e.lastAttempt.IsZero() {
		t := e.lastAttempt
		st.LastAttemptAt = &t
	}
	if !e.lastErrorAt.IsZero() {
		t := e.lastErrorAt
		st.LastErrorAt = &t
	}
	st.LastError = e.lastError
	st.ConsecutiveFail = e.consecutiveFail
	st.LoadedFromDisk = e.loadedFromDisk
	// A card is healthy when it has something to serve. A failing refresh
	// on top of a good render is a warning, not an outage.
	st.Healthy = st.HasRender
	return st
}

type diskMeta struct {
	ETag       string         `json:"etag"`
	RenderedAt time.Time      `json:"rendered_at"`
	Summary    map[string]any `json:"summary,omitempty"`
}

func (s *Store) persist(name string, snap *Snapshot) error {
	svgPath := filepath.Join(s.dir, name+".svg")
	metaPath := filepath.Join(s.dir, name+".json")

	if err := writeAtomic(svgPath, snap.SVG, 0o640); err != nil {
		return fmt.Errorf("persist %s: %w", svgPath, err)
	}
	meta, err := json.MarshalIndent(diskMeta{
		ETag:       snap.ETag,
		RenderedAt: snap.RenderedAt,
		Summary:    snap.Summary,
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode meta for %s: %w", name, err)
	}
	if err := writeAtomic(metaPath, meta, 0o640); err != nil {
		return fmt.Errorf("persist %s: %w", metaPath, err)
	}
	return nil
}

// Restore loads a previously persisted card. A missing file is not an error;
// it just means this is a first run.
func (s *Store) Restore(name string) (bool, error) {
	svgPath := filepath.Join(s.dir, name+".svg")
	svg, err := os.ReadFile(svgPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("read %s: %w", svgPath, err)
	}
	if len(svg) == 0 {
		return false, fmt.Errorf("%s is empty; refusing to serve it", svgPath)
	}

	snap := &Snapshot{SVG: svg, Gzip: gzipBytes(svg)}
	var meta diskMeta
	if raw, err := os.ReadFile(filepath.Join(s.dir, name+".json")); err == nil {
		if err := json.Unmarshal(raw, &meta); err == nil {
			snap.ETag = meta.ETag
			snap.RenderedAt = meta.RenderedAt
			snap.Summary = meta.Summary
		}
	}
	if snap.ETag == "" {
		sum := sha256.Sum256(svg)
		snap.ETag = `"` + hex.EncodeToString(sum[:16]) + `"`
	}
	if snap.RenderedAt.IsZero() {
		if fi, err := os.Stat(svgPath); err == nil {
			snap.RenderedAt = fi.ModTime()
		}
	}

	s.mu.Lock()
	e := s.get(name)
	e.snap = snap
	e.loadedFromDisk = true
	s.mu.Unlock()
	return true, nil
}

// gzipBytes compresses at the highest level. This runs once per refresh, so
// the extra CPU is free compared to the bytes it saves on every request.
// A failure here is not fatal: the identity encoding still serves.
func gzipBytes(data []byte) []byte {
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return nil
	}
	if _, err := zw.Write(data); err != nil {
		return nil
	}
	if err := zw.Close(); err != nil {
		return nil
	}
	return buf.Bytes()
}

// writeAtomic writes through a temporary file and renames, so a crash or a
// full disk can never leave a half-written card behind.
func writeAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		_ = f.Close()
		_ = os.Remove(tmp)
	}()

	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Chmod(perm); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
