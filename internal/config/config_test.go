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
      "timezone": "America/Toronto"
    }`))
	if err != nil {
		t.Fatal(err)
	}

	if cfg.LangsMaxAge.D() != time.Hour {
		t.Errorf("langs_max_age: got %s, want 1h", cfg.LangsMaxAge.D())
	}
	if cfg.StreakMaxAge.D() != 5*time.Minute {
		t.Errorf("streak_max_age: got %s, want 5m", cfg.StreakMaxAge.D())
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
          "username": "x", "timezone": "`+tz+`"
        }`))
		if err == nil {
			t.Errorf("timezone %q should be rejected", tz)
		}
	}
}

func TestRequiredFields(t *testing.T) {
	cases := map[string]string{
		"missing username": `{"timezone": "UTC"}`,
		"both cards off": `{"username":"x","timezone":"UTC",
            "languages":{"enabled":false},"streak":{"enabled":false}}`,
		"zero count": `{"username":"x","timezone":"UTC",
            "languages":{"count":0}}`,
		"bad weekday": `{"username":"x","timezone":"UTC",
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
      "username": "x", "timezone": "UTC",
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
      "username": "x", "timezone": "UTC",
      "langs_max_age": "6h", "streak_max_age": "90s"
    }`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LangsMaxAge.D() != 6*time.Hour {
		t.Errorf("got %s, want 6h", cfg.LangsMaxAge.D())
	}
	if cfg.StreakMaxAge.D() != 90*time.Second {
		t.Errorf("got %s, want 90s", cfg.StreakMaxAge.D())
	}

	for _, bad := range []string{`"nonsense"`, `"0s"`, `"-5m"`, `300`} {
		if _, err := config.Load(write(t, `{
          "username":"x","timezone":"UTC",
          "streak_max_age":`+bad+`}`)); err == nil {
			t.Errorf("streak_max_age %s should be rejected", bad)
		}
	}
}

func TestExcludedWeekdays(t *testing.T) {
	cfg, err := config.Load(write(t, `{
      "username":"x","timezone":"UTC",
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

// TestLoadBytes covers the path-free entry point Lambda mode uses.
func TestLoadBytes(t *testing.T) {
	cfg, err := config.LoadBytes([]byte(`{"username":"example-user","timezone":"UTC"}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Username != "example-user" {
		t.Errorf("username: got %q", cfg.Username)
	}
	for name, raw := range map[string][]byte{
		"empty":   nil,
		"invalid": []byte(`{"username":`),
	} {
		if _, err := config.LoadBytes(raw); err == nil {
			t.Errorf("%s input should be rejected", name)
		}
	}
}
