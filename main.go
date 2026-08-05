// Command gitdisplay renders the two SVG cards on a GitHub profile README.
//
// In Lambda mode (detected via AWS_LAMBDA_RUNTIME_API) each invocation
// fetches from GitHub, renders both cards, and uploads them to S3, where
// CloudFront serves them. A run that fails uploads nothing, so the previous
// cards stay in place; an empty or half-built card is never published.
// Without that variable it is a local CLI: -once renders both cards to
// files, -dump and -from record and replay the raw API data.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	// provided.al2023 ships no /usr/share/zoneinfo; without the embedded
	// tzdata the IANA timezone lookup fails on every Lambda cold start.
	_ "time/tzdata"

	"github.com/thesimpledev/gitdisplay/internal/config"
	"github.com/thesimpledev/gitdisplay/internal/gh"
	"github.com/thesimpledev/gitdisplay/internal/render"
	"github.com/thesimpledev/gitdisplay/internal/stats"
)

// Card names, also used as the uploaded and -once output filenames.
const (
	CardLangs  = "top-langs"
	CardStreak = "streak"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	var err error
	if os.Getenv("AWS_LAMBDA_RUNTIME_API") != "" {
		err = runLambda()
	} else {
		err = run()
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "gitdisplay: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		configPath  = flag.String("config", "/etc/gitdisplay/config.json", "path to the config file")
		once        = flag.Bool("once", false, "render both cards to files and exit")
		outDir      = flag.String("out", ".", "directory for -once output")
		dumpPath    = flag.String("dump", "", "write the raw fetched API data to this JSON file and exit")
		fromPath    = flag.String("from", "", "read a -dump file instead of calling the API")
		nowFlag     = flag.String("now", "", "treat this date as today (YYYY-MM-DD or RFC3339); for reproducible output")
		showVersion = flag.Bool("version", false, "print the version and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return nil
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}

	now := time.Now().In(cfg.Location())
	if *nowFlag != "" {
		now, err = parseNow(*nowFlag, cfg.Location())
		if err != nil {
			return err
		}
	}

	offline := *fromPath != ""
	oneShot := *once || *dumpPath != ""

	var client *gh.Client
	if !offline {
		token, err := githubToken(cfg)
		if err != nil {
			return err
		}
		client = gh.New(cfg.GitHub.Endpoint, token, cfg.GitHub.UserAgent,
			cfg.GitHub.Timeout.D(), cfg.GitHub.MaxRetries)
	}

	app := &app{cfg: cfg, client: client, now: now}

	if oneShot {
		return app.runOnce(*fromPath, *dumpPath, *outDir, *once)
	}
	if offline {
		return errors.New("-from only applies to -once or -dump")
	}
	return errors.New("nothing to do: use -once or -dump (server mode lives on the ec2-deploy branch)")
}

type app struct {
	cfg    config.Config
	client *gh.Client
	now    time.Time
}

// githubToken prefers the config document (the source of truth between
// deploys) and falls back to the environment for local runs.
func githubToken(cfg config.Config) (string, error) {
	token := strings.TrimSpace(cfg.GitHub.Token)
	if token == "" {
		token = strings.TrimSpace(os.Getenv("GITHUB_TOKEN"))
	}
	if token == "" {
		return "", errors.New("no GitHub token: set github.token in the config or GITHUB_TOKEN in the environment (repo and read:user scopes)")
	}
	return token, nil
}

// ---------------------------------------------------------------------------
// One-shot modes
// ---------------------------------------------------------------------------

func (a *app) runOnce(fromPath, dumpPath, outDir string, writeCards bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	var data *gh.Data
	var err error
	if fromPath != "" {
		data, err = gh.LoadDump(fromPath)
		if err != nil {
			return err
		}
		log.Printf("loaded %s (fetched %s)", fromPath, data.FetchedAt.Format(time.RFC3339))
	} else {
		data, err = a.client.Fetch(ctx, a.fetchOptions(a.cfg.Languages.Enabled, a.cfg.Streak.Enabled))
		if err != nil {
			return fmt.Errorf("fetch: %w", err)
		}
	}

	if dumpPath != "" {
		if err := writeJSON(dumpPath, data); err != nil {
			return err
		}
		log.Printf("wrote %s (%d repositories, %d calendars)",
			dumpPath, len(data.Repositories), len(data.Calendars))
	}

	if !writeCards {
		return nil
	}

	if err := os.MkdirAll(outDir, 0o755); err != nil { // #nosec G301 -- output directory for publicly published cards; browsable is intended
		return fmt.Errorf("create %s: %w", outDir, err)
	}

	if a.cfg.Languages.Enabled {
		svg, res := a.renderLangs(data)
		reportLangs(res)
		path := filepath.Join(outDir, CardLangs+".svg")
		if err := os.WriteFile(path, svg, 0o644); err != nil { // #nosec G306 -- the card is published publicly; world-readable is intended
			return fmt.Errorf("write %s: %w", path, err)
		}
		log.Printf("wrote %s", path)
	}

	if a.cfg.Streak.Enabled {
		res := stats.Streaks(data, a.cfg.Streak, a.cfg.Location(), a.now)
		reportStreak(res)
		path := filepath.Join(outDir, CardStreak+".svg")
		if err := os.WriteFile(path, render.Streak(res, a.cfg.Streak), 0o644); err != nil { // #nosec G306 -- the card is published publicly; world-readable is intended
			return fmt.Errorf("write %s: %w", path, err)
		}
		log.Printf("wrote %s", path)
	}
	return nil
}

// renderLangs picks the plain or split layout and returns the SVG together
// with the active-repository result, which is what gets reported and summarised.
func (a *app) renderLangs(data *gh.Data) ([]byte, stats.LangResult) {
	if a.cfg.Languages.SplitArchived {
		split := stats.LanguagesSplit(data, a.cfg.Languages)
		return render.LangsSplit(split, a.cfg.Languages), split.Active
	}
	res := stats.Languages(data, a.cfg.Languages)
	return render.Langs(res, a.cfg.Languages), res
}

func (a *app) fetchOptions(langs, streak bool) gh.Options {
	return gh.Options{
		Username:           a.cfg.Username,
		Location:           a.cfg.Location(),
		LanguagesPerRepo:   a.cfg.Languages.LanguagesPerRepo,
		FetchLanguages:     langs,
		FetchStreak:        streak,
		StartYear:          a.cfg.Streak.StartYear,
		Now:                a.now,
		OwnerAffiliations:  a.cfg.Languages.OwnerAffiliations,
		Organizations:      a.cfg.Languages.Organizations,
		Visibility:         a.cfg.Languages.Visibility,
		ExcludePaths:       a.cfg.Languages.ExcludePaths,
		GlobalExcludePaths: a.cfg.Languages.GlobalExcludePaths,
		ExcludeRepos:       a.cfg.Languages.ExcludeRepos,
		// When the split card is on, archived repositories are shown rather
		// than dropped, so their trees still need reading.
		ExcludeArchived:  a.cfg.Languages.ExcludeArchived && !a.cfg.Languages.SplitArchived,
		ResolveHackFiles: a.cfg.Languages.ResolveHackFiles,
		HackFileLimit:    a.cfg.Languages.HackFileLimit,
	}
}

// ---------------------------------------------------------------------------
// Reporting
// ---------------------------------------------------------------------------

// reportLangs writes config problems to stderr. A list entry that matches
// nothing is how a typo silently disables an exclusion, so it gets said out
// loud every refresh rather than being swallowed.
func reportLangs(res stats.LangResult) {
	for _, w := range res.Warnings {
		log.Printf("warning: %s", w)
	}
	for _, s := range res.Subtractions {
		note := ""
		if s.Clamped {
			note = " (capped at the reported total)"
		}
		log.Printf("path exclusion: %s removed %d bytes of %s across %d files%s",
			s.Repo, s.Bytes, s.Language, s.Files, note)
	}
	if n := len(res.ArchivedSkipped); n > 0 {
		shown, suffix := res.ArchivedSkipped, ""
		if len(shown) > 8 {
			shown, suffix = shown[:8], fmt.Sprintf(" and %d more", n-8)
		}
		log.Printf("skipped %d archived repositories: %s%s", n, strings.Join(shown, ", "), suffix)
	}
	log.Printf("languages: %d of %d repositories contributed, %d shown",
		res.ReposConsidered, res.ReposFetched, len(res.Languages))
}

func reportStreak(res stats.StreakResult) {
	for _, w := range res.Warnings {
		log.Printf("warning: %s", w)
	}
	log.Printf("streak: total %d, current %d, longest %d, %d days across %d years",
		res.TotalContributions, res.Current.Length, res.Longest.Length,
		res.DaysConsidered, len(res.Years))
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func parseNow(s string, loc *time.Location) (time.Time, error) {
	if t, err := time.ParseInLocation("2006-01-02", s, loc); err == nil {
		return t, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("-now %q: use YYYY-MM-DD or RFC3339", s)
	}
	return t.In(loc), nil
}

func writeJSON(path string, v any) error {
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("encode dump: %w", err)
	}
	raw = append(raw, '\n')
	// 0600: a dump can include private repository names and metadata.
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
