package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thesimpledev/gitdisplay/internal/config"
)

func write(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadAppliesDefaults(t *testing.T) {
	cfg, err := config.Load(write(t, `{
      "username": "example-user",
      "timezone": "America/Toronto",
      "state_dir": "/tmp/gitdisplay"
    }`))
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Languages.TTL.D() != 12*time.Hour {
		t.Errorf("languages ttl: got %s, want 12h", cfg.Languages.TTL.D())
	}
	if cfg.Streak.TTL.D() != 20*time.Minute {
		t.Errorf("streak ttl: got %s, want 20m", cfg.Streak.TTL.D())
	}
	// The paging fix is the default; the old behaviour has to be asked for.
	if cfg.Languages.MaxRepos != 0 {
		t.Errorf("max_repos should default to unlimited, got %d", cfg.Languages.MaxRepos)
	}
	if cfg.Languages.LanguagesPerRepo != 50 {
		t.Errorf("languages_per_repo: got %d, want 50", cfg.Languages.LanguagesPerRepo)
	}
	// Using every contribution year GitHub reports is the default.
	if cfg.Streak.StartYear != 0 {
		t.Errorf("start_year should default to 0, got %d", cfg.Streak.StartYear)
	}
	if cfg.Location().String() != "America/Toronto" {
		t.Errorf("timezone: got %s", cfg.Location())
	}
}

// TestTimezoneMustBeExplicit is the rule that keeps day boundaries, and so
// streaks, off the host's local time.
func TestTimezoneMustBeExplicit(t *testing.T) {
	for _, tz := range []string{"", "Local", "Mars/Olympus"} {
		_, err := config.Load(write(t, `{
          "username": "x", "state_dir": "/tmp/x", "timezone": "`+tz+`"
        }`))
		if err == nil {
			t.Errorf("timezone %q should be rejected", tz)
		}
	}
}

func TestRequiredFields(t *testing.T) {
	cases := map[string]string{
		"missing username":  `{"timezone": "UTC", "state_dir": "/tmp/x"}`,
		"missing state dir": `{"username": "x", "timezone": "UTC", "state_dir": ""}`,
		"both cards off": `{"username":"x","timezone":"UTC","state_dir":"/tmp/x",
            "languages":{"enabled":false},"streak":{"enabled":false}}`,
		"zero count": `{"username":"x","timezone":"UTC","state_dir":"/tmp/x",
            "languages":{"count":0}}`,
		"bad weekday": `{"username":"x","timezone":"UTC","state_dir":"/tmp/x",
            "streak":{"exclude_days":["Funday"]}}`,
	}
	for name, body := range cases {
		if _, err := config.Load(write(t, body)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

// TestUnknownFieldIsRejected turns a typo in the config into a startup failure
// rather than a setting that silently does nothing.
func TestUnknownFieldIsRejected(t *testing.T) {
	_, err := config.Load(write(t, `{
      "username": "x", "timezone": "UTC", "state_dir": "/tmp/x",
      "languages": {"exclude_repo": ["typo-in-the-key"]}
    }`))
	if err == nil {
		t.Fatal("an unknown config key should be rejected")
	}
	if !strings.Contains(err.Error(), "exclude_repo") {
		t.Errorf("the error should name the offending key, got %v", err)
	}
}

func TestDurationParsing(t *testing.T) {
	cfg, err := config.Load(write(t, `{
      "username": "x", "timezone": "UTC", "state_dir": "/tmp/x",
      "languages": {"ttl": "6h"}, "streak": {"ttl": "90s"}
    }`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Languages.TTL.D() != 6*time.Hour {
		t.Errorf("got %s, want 6h", cfg.Languages.TTL.D())
	}
	if cfg.Streak.TTL.D() != 90*time.Second {
		t.Errorf("got %s, want 90s", cfg.Streak.TTL.D())
	}

	for _, bad := range []string{`"nonsense"`, `"0s"`, `"-5m"`, `300`} {
		if _, err := config.Load(write(t, `{
          "username":"x","timezone":"UTC","state_dir":"/tmp/x",
          "streak":{"ttl":`+bad+`}}`)); err == nil {
			t.Errorf("ttl %s should be rejected", bad)
		}
	}
}

func TestExcludedWeekdays(t *testing.T) {
	cfg, err := config.Load(write(t, `{
      "username":"x","timezone":"UTC","state_dir":"/tmp/x",
      "streak":{"exclude_days":["Sat","sunday"]}}`))
	if err != nil {
		t.Fatal(err)
	}
	days := cfg.Streak.ExcludedWeekdays()
	if !days[time.Saturday] || !days[time.Sunday] {
		t.Errorf("got %v, want Saturday and Sunday", days)
	}
	if days[time.Monday] {
		t.Error("Monday should not be excluded")
	}
}

// TestExampleConfigIsValid keeps the shipped example from drifting out of sync
// with the parser.
func TestExampleConfigIsValid(t *testing.T) {
	if _, err := config.Load("../../config.example.json"); err != nil {
		t.Fatalf("config.example.json does not load: %v", err)
	}
}
