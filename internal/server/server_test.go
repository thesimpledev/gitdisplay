package server_test

import (
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/thesimpledev/gitdisplay/internal/cache"
	"github.com/thesimpledev/gitdisplay/internal/config"
	"github.com/thesimpledev/gitdisplay/internal/server"
)

func testConfig(t *testing.T) config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.Username = "example-user"
	cfg.Timezone = "UTC"
	cfg.StateDir = t.TempDir()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func newServer(t *testing.T) (*cache.Store, http.Handler, config.Config) {
	t.Helper()
	cfg := testConfig(t)
	store, err := cache.New(cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	return store, server.New(store, cfg, "test").Handler(), cfg
}

// TestCardUnavailableBeforeFirstRender checks that a card with no render
// returns an error rather than an empty image. A blank SVG in a README looks
// like a working card showing nothing.
func TestCardUnavailableBeforeFirstRender(t *testing.T) {
	_, h, _ := newServer(t)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/top-langs.svg", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("got %d, want 503", rec.Code)
	}
	if rec.Body.Len() > 0 && strings.Contains(rec.Body.String(), "<svg") {
		t.Error("should not return SVG markup when there is no render")
	}
}

func TestCardServesSnapshot(t *testing.T) {
	store, h, _ := newServer(t)
	svg := []byte(`<svg xmlns="http://www.w3.org/2000/svg"><text>hello</text></svg>`)
	if err := store.Publish(server.CardLangs, svg, nil, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/top-langs.svg", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "image/svg+xml; charset=utf-8" {
		t.Errorf("content type: got %q", got)
	}
	if rec.Body.String() != string(svg) {
		t.Errorf("body mismatch: got %q", rec.Body.String())
	}
	if rec.Header().Get("ETag") == "" {
		t.Error("missing ETag")
	}
	if rec.Header().Get("Last-Modified") == "" {
		t.Error("missing Last-Modified")
	}
	cc := rec.Header().Get("Cache-Control")
	// A revalidating cache is what gets a new card past camo; an immutable
	// or missing directive is what strands a stale one.
	if !strings.Contains(cc, "must-revalidate") || !strings.Contains(cc, "max-age=") {
		t.Errorf("cache-control should ask for revalidation, got %q", cc)
	}
}

// TestConditionalGet is what keeps a revalidating proxy cheap.
func TestConditionalGet(t *testing.T) {
	store, h, _ := newServer(t)
	if err := store.Publish(server.CardStreak, []byte("<svg/>"), nil, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	first := httptest.NewRecorder()
	h.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/streak.svg", nil))
	etag := first.Header().Get("ETag")

	req := httptest.NewRequest(http.MethodGet, "/streak.svg", nil)
	req.Header.Set("If-None-Match", etag)
	second := httptest.NewRecorder()
	h.ServeHTTP(second, req)

	if second.Code != http.StatusNotModified {
		t.Errorf("got %d, want 304", second.Code)
	}
	if second.Body.Len() != 0 {
		t.Error("304 must not carry a body")
	}

	// A weak validator prefix should still match.
	req = httptest.NewRequest(http.MethodGet, "/streak.svg", nil)
	req.Header.Set("If-None-Match", "W/"+etag)
	third := httptest.NewRecorder()
	h.ServeHTTP(third, req)
	if third.Code != http.StatusNotModified {
		t.Errorf("weak validator: got %d, want 304", third.Code)
	}
}

// TestETagChangesWithContent guards against a stale card being served forever
// under an unchanged validator.
func TestETagChangesWithContent(t *testing.T) {
	store, h, _ := newServer(t)
	now := time.Now().UTC()

	if err := store.Publish(server.CardLangs, []byte("<svg>one</svg>"), nil, now); err != nil {
		t.Fatal(err)
	}
	a := httptest.NewRecorder()
	h.ServeHTTP(a, httptest.NewRequest(http.MethodGet, "/top-langs.svg", nil))

	if err := store.Publish(server.CardLangs, []byte("<svg>two</svg>"), nil, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	b := httptest.NewRecorder()
	h.ServeHTTP(b, httptest.NewRequest(http.MethodGet, "/top-langs.svg", nil))

	if a.Header().Get("ETag") == b.Header().Get("ETag") {
		t.Error("ETag must change when the card changes")
	}
}

func TestGzipEncoding(t *testing.T) {
	store, h, _ := newServer(t)
	svg := []byte(strings.Repeat(`<rect x="0" y="0" width="205" height="8"/>`, 50))
	if err := store.Publish(server.CardLangs, svg, nil, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/top-langs.svg", nil)
	req.Header.Set("Accept-Encoding", "gzip, deflate")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("expected gzip, got %q", rec.Header().Get("Content-Encoding"))
	}
	if rec.Header().Get("Vary") != "Accept-Encoding" {
		t.Error("gzip responses must set Vary: Accept-Encoding")
	}
	if rec.Body.Len() >= len(svg) {
		t.Errorf("gzip body (%d) should be smaller than the original (%d)", rec.Body.Len(), len(svg))
	}

	zr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(svg) {
		t.Error("decompressed body does not match the original")
	}

	// The two representations must not share a validator.
	plain := httptest.NewRecorder()
	h.ServeHTTP(plain, httptest.NewRequest(http.MethodGet, "/top-langs.svg", nil))
	if plain.Header().Get("ETag") == rec.Header().Get("ETag") {
		t.Error("gzip and identity representations need different ETags")
	}
}

func TestHealthz(t *testing.T) {
	store, h, _ := newServer(t)

	// Nothing rendered yet: not ready.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("got %d, want 503 before any render", rec.Code)
	}

	now := time.Now().UTC()
	if err := store.Publish(server.CardLangs, []byte("<svg/>"),
		map[string]any{"total": 1}, now); err != nil {
		t.Fatal(err)
	}
	if err := store.Publish(server.CardStreak, []byte("<svg/>"), nil, now); err != nil {
		t.Fatal(err)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", rec.Code)
	}

	var body struct {
		Status string                  `json:"status"`
		Cards  map[string]cache.Status `json:"cards"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Status != "ok" {
		t.Errorf("status: got %q", body.Status)
	}
	langs := body.Cards[server.CardLangs]
	if langs.LastSuccessAt == nil {
		t.Error("healthz must report the last successful refresh per card")
	}

	// A failure after a good render keeps serving and shows up as the last
	// error, not as an outage.
	store.Fail(server.CardStreak, errTest{}, now.Add(time.Minute))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("a failed refresh on top of a good render should stay healthy, got %d", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Cards[server.CardStreak].LastError == "" {
		t.Error("healthz must report the last error")
	}
	if body.Cards[server.CardStreak].ConsecutiveFail != 1 {
		t.Error("healthz must count consecutive failures")
	}
}

type errTest struct{}

func (errTest) Error() string { return "github unreachable" }

func TestDisabledCardIsNotRouted(t *testing.T) {
	cfg := testConfig(t)
	cfg.Streak.Enabled = false
	store, err := cache.New(cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	h := server.New(store, cfg, "test").Handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/streak.svg", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("got %d, want 404 for a disabled card", rec.Code)
	}
}
