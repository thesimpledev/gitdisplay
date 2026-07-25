package cache_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/thesimpledev/gitdisplay/internal/cache"
)

func TestPublishAndSnapshot(t *testing.T) {
	dir := t.TempDir()
	store, err := cache.New(dir)
	if err != nil {
		t.Fatal(err)
	}

	if store.Snapshot("top-langs") != nil {
		t.Fatal("expected no snapshot before the first publish")
	}

	now := time.Now().UTC().Truncate(time.Second)
	svg := []byte("<svg>hello</svg>")
	if err := store.Publish("top-langs", svg, map[string]any{"n": 1}, now); err != nil {
		t.Fatal(err)
	}

	snap := store.Snapshot("top-langs")
	if snap == nil {
		t.Fatal("expected a snapshot")
	}
	if string(snap.SVG) != string(svg) {
		t.Errorf("svg mismatch: %q", snap.SVG)
	}
	if snap.ETag == "" || len(snap.Gzip) == 0 {
		t.Error("snapshot should carry an ETag and a pre-compressed body")
	}

	if _, err := os.Stat(filepath.Join(dir, "top-langs.svg")); err != nil {
		t.Errorf("card was not persisted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "top-langs.json")); err != nil {
		t.Errorf("metadata was not persisted: %v", err)
	}
}

// TestFailKeepsLastGoodRender is the rule that keeps a broken API from putting
// an empty card in a README.
func TestFailKeepsLastGoodRender(t *testing.T) {
	store, err := cache.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := store.Publish("streak", []byte("<svg>good</svg>"), nil, now); err != nil {
		t.Fatal(err)
	}

	store.Fail("streak", errors.New("api down"), now.Add(time.Minute))
	store.Fail("streak", errors.New("api down"), now.Add(2*time.Minute))

	snap := store.Snapshot("streak")
	if snap == nil || string(snap.SVG) != "<svg>good</svg>" {
		t.Fatal("a failed refresh must leave the previous render in place")
	}

	st := store.Status("streak")
	if !st.Healthy {
		t.Error("a card with a good render is still serving")
	}
	if st.ConsecutiveFail != 2 {
		t.Errorf("consecutive failures: got %d, want 2", st.ConsecutiveFail)
	}
	if st.LastError == "" || st.LastErrorAt == nil {
		t.Error("the last error should be recorded")
	}
	if st.LastSuccessAt == nil || !st.LastSuccessAt.Equal(now) {
		t.Error("the last success time should survive a failure")
	}
}

// TestRestoreAfterRestart is why the card is persisted at all: a reboot must
// not serve a blank card while the first refresh runs.
func TestRestoreAfterRestart(t *testing.T) {
	dir := t.TempDir()
	first, err := cache.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	svg := []byte("<svg>persisted</svg>")
	if err := first.Publish("top-langs", svg, map[string]any{"k": "v"}, now); err != nil {
		t.Fatal(err)
	}
	wantETag := first.Snapshot("top-langs").ETag

	// A fresh process over the same directory.
	second, err := cache.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := second.Restore("top-langs")
	if err != nil {
		t.Fatal(err)
	}
	if !restored {
		t.Fatal("expected the card to be restored")
	}

	snap := second.Snapshot("top-langs")
	if string(snap.SVG) != string(svg) {
		t.Errorf("restored svg mismatch: %q", snap.SVG)
	}
	if snap.ETag != wantETag {
		t.Errorf("ETag should survive a restart: got %q, want %q", snap.ETag, wantETag)
	}
	if !snap.RenderedAt.Equal(now) {
		t.Errorf("render time should survive a restart: got %v, want %v", snap.RenderedAt, now)
	}
	if len(snap.Gzip) == 0 {
		t.Error("restored card should be recompressed for serving")
	}

	st := second.Status("top-langs")
	if !st.LoadedFromDisk {
		t.Error("status should show the card came from disk")
	}
	// It has not refreshed yet in this process, so there is no success time.
	if st.LastSuccessAt != nil {
		t.Error("restoring is not a successful refresh")
	}
}

func TestRestoreMissingIsNotAnError(t *testing.T) {
	store, err := cache.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	restored, err := store.Restore("top-langs")
	if err != nil {
		t.Errorf("a first run should not be an error: %v", err)
	}
	if restored {
		t.Error("nothing should have been restored")
	}
}

// TestRestoreRefusesEmptyFile guards the one case where persistence could
// itself produce a blank card.
func TestRestoreRefusesEmptyFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "streak.svg"), nil, 0o640); err != nil {
		t.Fatal(err)
	}
	store, err := cache.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Restore("streak"); err == nil {
		t.Error("an empty saved card should be refused, not served")
	}
	if store.Snapshot("streak") != nil {
		t.Error("an empty card must not become a snapshot")
	}
}
