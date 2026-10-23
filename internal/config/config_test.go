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

// TestValidateErrors pins the message for every rejected setting, so an
// operator reading the Lambda log sees exactly which key is wrong.
func TestValidateErrors(t *testing.T) {
	cases := map[string]struct{ body, want string }{
		"username":       {`{"timezone":"UTC"}`, "username is required"},
		"timezone empty": {`{"username":"x","timezone":" "}`, "timezone is required"},
		"timezone bad":   {`{"username":"x","timezone":"Mars/Olympus"}`, `timezone "Mars/Olympus"`},
		"timezone local": {`{"username":"x","timezone":"Local"}`, `timezone must be an explicit IANA name, not "Local"`},
		"count":          {`{"username":"x","timezone":"UTC","languages":{"count":0}}`, "languages.count must be at least 1"},
		"per repo":       {`{"username":"x","timezone":"UTC","languages":{"languages_per_repo":0}}`, "languages.languages_per_repo must be at least 1"},
		"max repos":      {`{"username":"x","timezone":"UTC","languages":{"max_repos":-1}}`, "languages.max_repos cannot be negative"},
		"visibility":     {`{"username":"x","timezone":"UTC","languages":{"visibility":"secret"}}`, `languages.visibility must be "all" or "public", got "secret"`},
		"affiliation":    {`{"username":"x","timezone":"UTC","languages":{"owner_affiliations":["FRIEND"]}}`, `languages.owner_affiliations: "FRIEND" is not a GitHub repository affiliation`},
		"retries":        {`{"username":"x","timezone":"UTC","github":{"max_retries":-1}}`, "github.max_retries cannot be negative"},
		"weekday":        {`{"username":"x","timezone":"UTC","streak":{"exclude_days":["Funday"]}}`, `streak.exclude_days: "Funday" is not a weekday name`},
		"both off":       {`{"username":"x","timezone":"UTC","languages":{"enabled":false},"streak":{"enabled":false}}`, "both cards are disabled; nothing to render"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := config.LoadBytes([]byte(tc.body))
			if err == nil {
				t.Fatalf("expected an error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("got %q, want it to contain %q", err, tc.want)
			}
		})
	}
}

// TestValidateAcceptsEveryAllowedValue covers the accepted spellings of the
// enumerated settings.
func TestValidateAcceptsEveryAllowedValue(t *testing.T) {
	body := `{"username":"x","timezone":"UTC",
	  "languages":{"visibility":" Public ","owner_affiliations":["owner"," ORGANIZATION_MEMBER ","Collaborator"]},
	  "streak":{"exclude_days":["sun","Monday","tues","weds","thurs","fri","SATURDAY"]}}`
	cfg, err := config.LoadBytes([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if got := len(cfg.Streak.ExcludedWeekdays()); got != 7 {
		t.Errorf("excluded weekdays: got %d, want 7", got)
	}
	for _, vis := range []string{"", "all"} {
		raw := `{"username":"x","timezone":"UTC","languages":{"visibility":"` + vis + `"}}`
		if _, err := config.LoadBytes([]byte(raw)); err != nil {
			t.Errorf("visibility %q: %v", vis, err)
		}
	}
	only := `{"username":"x","timezone":"UTC","languages":{"enabled":false}}`
	if _, err := config.LoadBytes([]byte(only)); err != nil {
		t.Errorf("streak card alone should be valid: %v", err)
	}
}
