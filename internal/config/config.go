// Package config loads and validates the configuration.
//
// Everything that shapes a card — which repos to skip, which languages to
// hide, which vendored byte counts to zero out — lives in one JSON document,
// read from a local file (Load) or from bytes fetched elsewhere (LoadBytes).
// None of it is accepted from the query string, so the card URL in a README
// never discloses the exclusion list.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// Duration wraps time.Duration so it can be written in JSON as "12h".
type Duration time.Duration

// UnmarshalJSON parses a duration string such as "12h".
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a string such as \"12h\": %w", err)
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("bad duration %q: %w", s, err)
	}
	if parsed <= 0 {
		return fmt.Errorf("duration %q must be positive", s)
	}
	*d = Duration(parsed)
	return nil
}

// MarshalJSON writes the duration back in the same string form.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// D returns the wrapped time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// Config is the whole service configuration.
type Config struct {
	// Username is the GitHub login the cards describe.
	Username string `json:"username"`

	// Timezone is an explicit IANA name, for example "America/Toronto".
	// The machine's local time is never used; day boundaries decide streaks,
	// so this has to be pinned rather than inherited from the host.
	Timezone string `json:"timezone"`

	// LangsMaxAge and StreakMaxAge are the Cache-Control max-age set on each
	// uploaded card. See doc/CACHING.md: GitHub proxies README images through
	// camo, which keeps its own copy, so these are chosen to make camo
	// revalidate rather than to fight it.
	LangsMaxAge  Duration `json:"langs_max_age"`
	StreakMaxAge Duration `json:"streak_max_age"`

	GitHub    GitHub    `json:"github"`
	Languages Languages `json:"languages"`
	Streak    Streak    `json:"streak"`

	// loc is the resolved Timezone.
	loc *time.Location
}

// Location returns the resolved timezone. Valid after Load or Validate.
func (c *Config) Location() *time.Location { return c.loc }

// GitHub controls the outbound API calls. The only destination is the
// GraphQL endpoint; no other host is ever contacted.
type GitHub struct {
	Endpoint   string   `json:"endpoint"`
	Timeout    Duration `json:"timeout"`
	MaxRetries int      `json:"max_retries"`
	UserAgent  string   `json:"user_agent"`

	// Token authenticates the API calls (repo and read:user scopes). When
	// empty, the GITHUB_TOKEN environment variable is used instead. The
	// config document lives in a private bucket, never in git, which is
	// what makes carrying it here acceptable.
	Token string `json:"token"`
}

// Languages configures the top-languages card.
type Languages struct {
	Enabled bool   `json:"enabled"`
	Title   string `json:"title"`

	// Count is how many languages appear on the card.
	Count int `json:"count"`

	// SplitArchived renders one card in two columns: active repositories on
	// the left, archived ones on the right. It is a look at what you work on
	// now against what you used to.
	//
	// Each section is ranked and given percentages against itself, so the
	// archived half is read on its own terms rather than being swamped by the
	// larger active half. ExcludeArchived is ignored when this is on, since
	// the whole point is to show both.
	SplitArchived bool `json:"split_archived"`

	// ActiveLabel and ArchivedLabel head the two sections.
	ActiveLabel   string `json:"active_label"`
	ArchivedLabel string `json:"archived_label"`

	// ArchivedCount is how many languages the archived section shows.
	// 0 uses Count.
	ArchivedCount int `json:"archived_count"`

	// LanguageAliases merges one language into another before ranking, keyed
	// source -> target and compared case-insensitively. For example
	// {"TSQL": "SQL", "PLpgSQL": "SQL"} collapses the SQL dialects GitHub
	// reports separately into a single row.
	//
	// GitHub's linguist really does treat these as different languages, so
	// this is a presentation choice rather than a correction. The merged row
	// takes the colour of whichever source contributed the most bytes.
	LanguageAliases map[string]string `json:"language_aliases"`

	// HideLanguages drops languages by name, compared case-insensitively.
	// An entry matches either the name GitHub reported or the name it was
	// aliased to.
	// Applied before Count is taken, so a hidden language does not consume
	// one of the displayed slots.
	HideLanguages []string `json:"hide_languages"`

	// ExcludeRepos drops whole repositories. An entry matches either the bare
	// name ("some-repo") or the qualified form ("some-owner/some-repo"). With
	// organizations in play a bare name is no longer unique, so prefer the
	// qualified form.
	ExcludeRepos []string `json:"exclude_repos"`

	// OwnerAffiliations is the ownerAffiliations argument used for your own
	// repositories. OWNER is what you want; ORGANIZATION_MEMBER only returns
	// the repositories reachable through team membership, which is a much
	// smaller set than an organization actually holds. Use Organizations to
	// pull in an organization properly.
	OwnerAffiliations []string `json:"owner_affiliations"`

	// Organizations lists organization logins whose repositories count
	// towards the card, each queried directly.
	//
	// Every byte of a listed organization's repositories is attributed to
	// you. That is the right call for an organization whose code you wrote,
	// and the wrong one for an organization you merely belong to.
	Organizations []string `json:"organizations"`

	// Visibility is "all" or "public". "all" counts private repositories,
	// which is what the card being replaced does. The card only ever shows
	// language names and percentages, never repository names.
	Visibility string `json:"visibility"`

	// ExcludeArchived drops every archived repository. Archiving a project on
	// GitHub then becomes the way to retire it from the card, which leaves
	// ExcludeRepos for the handful of cases archiving does not cover.
	//
	// Forks are already left out by the query itself, so they never need an
	// entry here.
	ExcludeArchived bool `json:"exclude_archived"`

	// ByteOverrides replaces the byte count GitHub reports for one language
	// in one repository. Set a language to 0 to remove vendored or purchased
	// code that GitHub attributes to you. Keyed repo -> language -> bytes.
	//
	// Language keys are matched case-insensitively. This is the exact
	// escape hatch: whatever number you write is the number used.
	ByteOverrides map[string]map[string]int64 `json:"byte_overrides"`

	// ExcludePaths drops directories inside a repository you otherwise want
	// to keep. Keyed repo -> list of path prefixes, for example
	// {"some-repo": ["themes/purchased-theme/"]}.
	//
	// GitHub's language API reports one byte total per repository with no
	// breakdown by directory, so there is nothing to subtract from it
	// directly. For each repository listed here the service reads the git
	// tree, sums the blob sizes under each prefix, works out which language
	// each file belongs to, and subtracts that from the repository's totals.
	//
	// The result is an approximation of what GitHub's own linguist would
	// have counted. Every subtraction is logged on every run so you can
	// check it. Where you need an exact number, use
	// ByteOverrides instead.
	ExcludePaths map[string][]string `json:"exclude_paths"`

	// GlobalExcludePaths applies path prefixes to every repository, rather
	// than naming one repository at a time in ExcludePaths.
	//
	// Note what this is and is not for. GitHub's linguist already leaves
	// vendor/, node_modules/, bower_components/ and third_party/ out of a
	// repository's language totals, so listing those changes nothing: there
	// are no bytes there to take away. What this catches is the directories
	// linguist does count and you do not want, such as a purchased
	// WordPress theme under themes/.
	//
	// Setting this makes a refresh read the git tree of every repository that
	// counts towards the card, which is one extra API call each. That is fine
	// on a twelve-hour refresh and is never on the request path.
	GlobalExcludePaths []string `json:"global_exclude_paths"`

	// ResolveHackFiles controls the .php special case. GitHub labels a .php
	// file with no "<?php" opening tag as Hack, not PHP. When a path
	// exclusion covers .php files, the service reads those blobs to decide
	// which of the two to subtract from. Bounded by HackFileLimit.
	ResolveHackFiles bool `json:"resolve_hack_files"`

	// HackFileLimit caps how many .php blobs are read for the check above.
	HackFileLimit int `json:"hack_file_limit"`

	// PercentOfDisplayed divides each percentage by the sum of the displayed
	// languages, so the card adds up to 100%. When false, percentages are of
	// all non-hidden languages and the card sums to less than 100%.
	PercentOfDisplayed bool `json:"percent_of_displayed"`

	// MaxRepos caps how many repositories are read, in the order GitHub
	// returns them. 0 means every repository. This exists to reproduce the
	// unpaginated behaviour of the original tool; leave it at 0.
	MaxRepos int `json:"max_repos"`

	// LanguagesPerRepo is how many languages are requested per repository,
	// largest first. The original asked for 10 and lost the tail.
	LanguagesPerRepo int `json:"languages_per_repo"`

	Theme Theme `json:"theme"`
}

// Streak configures the contribution streak card.
type Streak struct {
	Enabled bool `json:"enabled"`

	// ExcludeDays lists weekday names ("Sun", "Sat") that do not break a
	// streak. An excluded day extends a streak that is already running but
	// never starts one.
	ExcludeDays []string `json:"exclude_days"`

	// StartYear, when non-zero, ignores contribution years before it.
	// 0 uses every year GitHub reports, which is what you want: the account
	// creation year is not the first year with contributions.
	StartYear int `json:"start_year"`

	Theme StreakTheme `json:"theme"`
}

// Theme is the palette for the languages card.
type Theme struct {
	Title      string `json:"title"`
	Text       string `json:"text"`
	Background string `json:"background"`
	Border     string `json:"border"`
	// Track is the unfilled part of each language bar.
	Track string `json:"track"`
	// HideBorder removes the card outline.
	HideBorder bool `json:"hide_border"`
}

// StreakTheme is the palette for the streak card.
type StreakTheme struct {
	Background string `json:"background"`
	Border     string `json:"border"`
	Text       string `json:"text"`
	Label      string `json:"label"`
	Dates      string `json:"dates"`
	// Accent colours the fire icon, the ring and the current-streak label.
	Accent     string `json:"accent"`
	Divider    string `json:"divider"`
	HideBorder bool   `json:"hide_border"`
}

// Default returns a configuration with every field populated.
func Default() Config {
	return Config{
		Timezone:     "UTC",
		LangsMaxAge:  Duration(1 * time.Hour),
		StreakMaxAge: Duration(5 * time.Minute),
		GitHub: GitHub{
			Endpoint:   "https://api.github.com/graphql",
			Timeout:    Duration(30 * time.Second),
			MaxRetries: 4,
			UserAgent:  "gitdisplay/1.0 (+https://github.com/thesimpledev/gitdisplay)",
		},
		Languages: Languages{
			Enabled:            true,
			Title:              "Most Used Languages",
			Count:              10,
			ActiveLabel:        "Active",
			ArchivedLabel:      "Archived",
			PercentOfDisplayed: true,
			MaxRepos:           0,
			LanguagesPerRepo:   50,
			ResolveHackFiles:   true,
			HackFileLimit:      200,
			OwnerAffiliations:  []string{"OWNER"},
			Visibility:         "all",
			Theme: Theme{
				Title:      "#ffb000",
				Text:       "#ffffff",
				Background: "#000000",
				Border:     "#e4e2e2",
				Track:      "#dddddd",
			},
		},
		Streak: Streak{
			Enabled: true,
			Theme: StreakTheme{
				Background: "#000000",
				Border:     "#e4e2e2",
				Text:       "#fefefe",
				Label:      "#fefefe",
				Dates:      "#9e9e9e",
				Accent:     "#fb8c00",
				Divider:    "#e4e2e2",
			},
		},
	}
}

// Load reads a JSON config file over the defaults and validates the result.
func Load(path string) (Config, error) {
	raw, err := os.ReadFile(path) // #nosec G304 -- the path is the operator's own -config CLI argument; reading it is the point
	if err != nil {
		return Default(), fmt.Errorf("read config: %w", err)
	}
	cfg, err := LoadBytes(raw)
	if err != nil {
		return cfg, fmt.Errorf("config %s: %w", path, err)
	}
	return cfg, nil
}

// LoadBytes decodes JSON config bytes over the defaults and validates the
// result. It is the path-free half of Load, for config that arrives from
// somewhere other than the local filesystem.
func LoadBytes(raw []byte) (Config, error) {
	cfg := Default()

	if len(raw) == 0 {
		return cfg, fmt.Errorf("config is empty")
	}

	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("parse: %w", err)
	}

	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// Validate checks the configuration and resolves the timezone.
func (c *Config) Validate() error {
	if strings.TrimSpace(c.Username) == "" {
		return fmt.Errorf("username is required")
	}
	if strings.TrimSpace(c.Timezone) == "" {
		return fmt.Errorf("timezone is required and must be an IANA name such as \"America/Toronto\"")
	}
	loc, err := time.LoadLocation(c.Timezone)
	if err != nil {
		return fmt.Errorf("timezone %q: %w", c.Timezone, err)
	}
	if c.Timezone == "Local" {
		return fmt.Errorf("timezone must be an explicit IANA name, not %q", c.Timezone)
	}
	c.loc = loc

	if c.Languages.Count < 1 {
		return fmt.Errorf("languages.count must be at least 1")
	}
	if c.Languages.LanguagesPerRepo < 1 {
		return fmt.Errorf("languages.languages_per_repo must be at least 1")
	}
	if c.Languages.MaxRepos < 0 {
		return fmt.Errorf("languages.max_repos cannot be negative")
	}
	switch strings.ToLower(strings.TrimSpace(c.Languages.Visibility)) {
	case "", "all", "public":
	default:
		return fmt.Errorf("languages.visibility must be \"all\" or \"public\", got %q", c.Languages.Visibility)
	}
	for _, a := range c.Languages.OwnerAffiliations {
		switch strings.ToUpper(strings.TrimSpace(a)) {
		case "OWNER", "ORGANIZATION_MEMBER", "COLLABORATOR":
		default:
			return fmt.Errorf("languages.owner_affiliations: %q is not a GitHub repository affiliation", a)
		}
	}
	if c.GitHub.MaxRetries < 0 {
		return fmt.Errorf("github.max_retries cannot be negative")
	}
	for _, day := range c.Streak.ExcludeDays {
		if _, ok := weekdayByName(day); !ok {
			return fmt.Errorf("streak.exclude_days: %q is not a weekday name (use Sun, Mon, Tue, Wed, Thu, Fri, Sat)", day)
		}
	}
	if !c.Languages.Enabled && !c.Streak.Enabled {
		return fmt.Errorf("both cards are disabled; nothing to render")
	}
	return nil
}

// ExcludedWeekdays resolves ExcludeDays into a set.
func (s Streak) ExcludedWeekdays() map[time.Weekday]bool {
	if len(s.ExcludeDays) == 0 {
		return nil
	}
	out := make(map[time.Weekday]bool, len(s.ExcludeDays))
	for _, name := range s.ExcludeDays {
		if wd, ok := weekdayByName(name); ok {
			out[wd] = true
		}
	}
	return out
}

func weekdayByName(name string) (time.Weekday, bool) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "sun", "sunday":
		return time.Sunday, true
	case "mon", "monday":
		return time.Monday, true
	case "tue", "tues", "tuesday":
		return time.Tuesday, true
	case "wed", "weds", "wednesday":
		return time.Wednesday, true
	case "thu", "thur", "thurs", "thursday":
		return time.Thursday, true
	case "fri", "friday":
		return time.Friday, true
	case "sat", "saturday":
		return time.Saturday, true
	}
	return 0, false
}
