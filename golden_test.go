package main

import (
	"bytes"
	"flag"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thesimpledev/gitdisplay/internal/config"
)

var update = flag.Bool("update", false, "rewrite the golden files from the current output")

const goldenDump = "testdata/golden/dump.json"

// goldenCases are two configurations that between them reach every
// language and streak option, rendered from one synthetic dump.
var goldenCases = map[string]string{
	"split": `{
  "username": "example-user",
  "timezone": "America/New_York",
  "languages": {
    "count": 4,
    "split_archived": true,
    "archived_count": 3,
    "exclude_repos": ["eta", "some-org/epsilonx", "", "zeta"],
    "hide_languages": ["HTML", "css", "Cobol", ""],
    "language_aliases": {"TSQL": "SQL", "PLpgSQL": "SQL"},
    "byte_overrides": {"delta": {"Rust": 15000, "Cobol": 1}, "nosuch": {"Go": 1}},
    "exclude_paths": {"gamma": ["themes/bought/", "missing/", " "], "theta": ["lib/"], "ghost": ["x/"], "beta": ["src/"]}
  },
  "streak": {"exclude_days": []}
}`,
	"plain": `{
  "username": "example-user",
  "timezone": "America/New_York",
  "languages": {
    "count": 5,
    "exclude_archived": true,
    "exclude_repos": ["zeta", "example-user/eta"],
    "percent_of_displayed": false,
    "max_repos": 8,
    "languages_per_repo": 2,
    "global_exclude_paths": ["dist/"],
    "theme": {"hide_border": true, "title": "#111111", "text": "#222222",
      "background": "#333333", "border": "#444444", "track": "#555555"}
  },
  "streak": {
    "exclude_days": ["Sat", "Sun"],
    "start_year": 2025,
    "theme": {"hide_border": true, "background": "#101010", "border": "#202020",
      "text": "#303030", "label": "#404040", "dates": "#505050",
      "accent": "#606060", "divider": "#707070"}
  }
}`,
}

// TestGoldenCards renders both cards from a fixed dump and compares the SVGs
// and the log output byte for byte with the recorded versions. Any change in
// what a card shows, or in what a run reports, fails this test.
func TestGoldenCards(t *testing.T) {
	for name, raw := range goldenCases {
		t.Run(name, func(t *testing.T) {
			got := renderGolden(t, raw)
			dir := filepath.Join("testdata", "golden", name)
			for file, content := range got {
				compareGolden(t, filepath.Join(dir, file), content)
			}
		})
	}
}

func renderGolden(t *testing.T, raw string) map[string][]byte {
	t.Helper()
	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "config.json")
	if err := os.WriteFile(cfgPath, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	now, err := time.ParseInLocation("2006-01-02 15:04", "2026-03-10 09:30", cfg.Location())
	if err != nil {
		t.Fatal(err)
	}

	var logs bytes.Buffer
	log.SetOutput(&logs)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(os.Stderr)
		log.SetFlags(log.LstdFlags)
	})

	outDir := filepath.Join(tmp, "out")
	a := &app{cfg: cfg, now: now}
	if err := a.runOnce(goldenDump, "", outDir, true); err != nil {
		t.Fatal(err)
	}

	out := map[string][]byte{
		"log.txt": []byte(strings.ReplaceAll(logs.String(), outDir, "OUT")),
	}
	for _, card := range []string{CardLangs, CardStreak} {
		svg, err := os.ReadFile(filepath.Join(outDir, card+".svg")) // #nosec G304 -- test output in a temp dir
		if err != nil {
			t.Fatal(err)
		}
		out[card+".svg"] = svg
	}
	return out
}

func compareGolden(t *testing.T, path string, got []byte) {
	t.Helper()
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path) // #nosec G304 -- fixed golden path under testdata
	if err != nil {
		t.Fatalf("%v (run go test -run TestGoldenCards -update to record it)", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs from the recorded output\n--- got ---\n%s\n--- want ---\n%s", path, got, want)
	}
}

// TestRunOnceDumpOnly replays the fixture into a dump without rendering, so
// the dump path writes a file that loads back to the same data and no card.
func TestRunOnceDumpOnly(t *testing.T) {
	cfg, err := config.LoadBytes([]byte(`{"username":"example-user","timezone":"UTC"}`))
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	dump := filepath.Join(tmp, "nested", "dump.json")
	outDir := filepath.Join(tmp, "out")

	log.SetOutput(&bytes.Buffer{})
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	a := &app{cfg: cfg, now: time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC)}
	if err := a.runOnce(goldenDump, dump, outDir, false); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(dump)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("dump permissions: got %o, want 600", perm)
	}
	if _, err := os.Stat(outDir); !os.IsNotExist(err) {
		t.Errorf("no output directory should be created without -once, got %v", err)
	}

	want, err := os.ReadFile(goldenDump)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dump) // #nosec G304 -- test output in a temp dir
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(got, []byte(`"username": "example-user"`)) || len(got) < len(want)/2 {
		t.Errorf("dump does not look like the replayed fixture:\n%s", got)
	}
}

// TestRunOnceReportsBadInput covers the failures a one-shot run can hit
// before anything is written.
func TestRunOnceReportsBadInput(t *testing.T) {
	cfg, err := config.LoadBytes([]byte(`{"username":"example-user","timezone":"UTC"}`))
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	a := &app{cfg: cfg, now: time.Now()}

	if err := a.runOnce(filepath.Join(tmp, "missing.json"), "", tmp, true); err == nil {
		t.Error("a missing -from file should fail")
	}

	blocker := filepath.Join(tmp, "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	log.SetOutput(&bytes.Buffer{})
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	if err := a.runOnce(goldenDump, "", filepath.Join(blocker, "out"), true); err == nil {
		t.Error("an output directory under a file should fail")
	}
}
