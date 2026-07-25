// Command gitdisplay serves the two SVG cards on a GitHub profile README.
//
// A background goroutine refreshes each card on its own schedule and keeps the
// rendered bytes in memory. Requests never touch GitHub, so they answer in
// well under a millisecond. A refresh that fails leaves the previous card in
// place; an empty or half-built card is never served.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/thesimpledev/gitdisplay/internal/cache"
	"github.com/thesimpledev/gitdisplay/internal/config"
	"github.com/thesimpledev/gitdisplay/internal/gh"
	"github.com/thesimpledev/gitdisplay/internal/render"
	"github.com/thesimpledev/gitdisplay/internal/server"
	"github.com/thesimpledev/gitdisplay/internal/stats"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := run(); err != nil {
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
		addrFlag    = flag.String("addr", "", "override server.addr from the config")
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
	if *addrFlag != "" {
		cfg.Server.Addr = *addrFlag
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
		token := os.Getenv("GITHUB_TOKEN")
		if token == "" {
			return errors.New("GITHUB_TOKEN is not set (needs the repo and read:user scopes)")
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
	return app.serve()
}

type app struct {
	cfg    config.Config
	client *gh.Client
	store  *cache.Store
	now    time.Time
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

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", outDir, err)
	}

	if a.cfg.Languages.Enabled {
		svg, res := a.renderLangs(data)
		reportLangs(res)
		path := filepath.Join(outDir, server.CardLangs+".svg")
		if err := os.WriteFile(path, svg, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
		log.Printf("wrote %s", path)
	}

	if a.cfg.Streak.Enabled {
		res := stats.Streaks(data, a.cfg.Streak, a.cfg.Location(), a.now)
		reportStreak(res)
		path := filepath.Join(outDir, server.CardStreak+".svg")
		if err := os.WriteFile(path, render.Streak(res, a.cfg.Streak), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
		log.Printf("wrote %s", path)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Server mode
// ---------------------------------------------------------------------------

func (a *app) serve() error {
	store, err := cache.New(a.cfg.StateDir)
	if err != nil {
		return err
	}
	a.store = store

	// Serve the last good render straight away rather than a blank card while
	// the first refresh runs.
	for _, name := range a.enabledCards() {
		restored, err := store.Restore(name)
		switch {
		case err != nil:
			log.Printf("warning: could not restore %s from disk: %v", name, err)
		case restored:
			log.Printf("restored %s from %s", name, a.cfg.StateDir)
		default:
			log.Printf("no saved %s on disk; it will be blank until the first refresh completes", name)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if a.cfg.Languages.Enabled {
		go a.refreshLoop(ctx, server.CardLangs, a.cfg.Languages.TTL.D(), a.refreshLangs)
	}
	if a.cfg.Streak.Enabled {
		go a.refreshLoop(ctx, server.CardStreak, a.cfg.Streak.TTL.D(), a.refreshStreak)
	}

	srv := &http.Server{
		Addr:              a.cfg.Server.Addr,
		Handler:           server.New(store, a.cfg, version).Handler(),
		ReadTimeout:       a.cfg.Server.ReadTimeout.D(),
		ReadHeaderTimeout: a.cfg.Server.ReadTimeout.D(),
		WriteTimeout:      a.cfg.Server.WriteTimeout.D(),
		IdleTimeout:       a.cfg.Server.IdleTimeout.D(),
		MaxHeaderBytes:    16 << 10,
	}

	errCh := make(chan error, 1)
	var redirectSrv *http.Server

	if a.cfg.Server.TLSEnabled() {
		reloader, err := server.NewCertReloader(a.cfg.Server.TLSCertFile, a.cfg.Server.TLSKeyFile)
		if err != nil {
			return err
		}
		srv.TLSConfig = server.TLSConfig(reloader)

		// certbot renews in place; SIGHUP forces an immediate reload without
		// waiting for the periodic check.
		hup := make(chan os.Signal, 1)
		signal.Notify(hup, syscall.SIGHUP)
		go func() {
			for range hup {
				if err := reloader.Reload(); err != nil {
					log.Printf("warning: SIGHUP certificate reload failed: %v", err)
				} else {
					log.Print("reloaded TLS certificate on SIGHUP")
				}
			}
		}()

		if a.cfg.Server.RedirectHTTP {
			redirectSrv = &http.Server{
				Addr: a.cfg.Server.RedirectAddr,
				Handler: server.RedirectHandler(
					a.cfg.Server.PublicHost, a.cfg.Server.ACMEWebroot),
				ReadHeaderTimeout: a.cfg.Server.ReadTimeout.D(),
				WriteTimeout:      a.cfg.Server.WriteTimeout.D(),
				MaxHeaderBytes:    16 << 10,
			}
			rln, err := net.Listen("tcp", a.cfg.Server.RedirectAddr)
			if err != nil {
				return fmt.Errorf("listen on %s: %w", a.cfg.Server.RedirectAddr, err)
			}
			log.Printf("redirecting HTTP on %s to https://%s", rln.Addr(), a.cfg.Server.PublicHost)
			go func() {
				if err := redirectSrv.Serve(rln); err != nil && !errors.Is(err, http.ErrServerClosed) {
					errCh <- err
				}
			}()
		}
	}

	ln, err := net.Listen("tcp", a.cfg.Server.Addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", a.cfg.Server.Addr, err)
	}

	scheme := "http"
	if a.cfg.Server.TLSEnabled() {
		scheme = "https"
	}
	log.Printf("gitdisplay %s serving %s on %s (user %s, timezone %s)",
		version, scheme, ln.Addr(), a.cfg.Username, a.cfg.Timezone)

	go func() {
		var err error
		if a.cfg.Server.TLSEnabled() {
			// The certificate comes from TLSConfig.GetCertificate, so the
			// file arguments stay empty.
			err = srv.ServeTLS(ln, "", "")
		} else {
			err = srv.Serve(ln)
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		log.Print("shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if redirectSrv != nil {
		_ = redirectSrv.Shutdown(shutdownCtx)
	}
	return srv.Shutdown(shutdownCtx)
}

// refreshLoop keeps one card up to date.
//
// A failed refresh is retried sooner than the normal interval, backing off so
// a sustained GitHub outage does not turn into a request flood. The card keeps
// serving its last good render throughout.
func (a *app) refreshLoop(ctx context.Context, name string, ttl time.Duration, refresh func(context.Context) error) {
	var failures int

	for {
		start := time.Now()
		err := refresh(ctx)
		if ctx.Err() != nil {
			return
		}

		next := ttl
		if err != nil {
			failures++
			a.store.Fail(name, err, time.Now().UTC())
			next = retryDelay(failures, ttl)
			log.Printf("refresh %s failed (attempt %d, retrying in %s): %v",
				name, failures, next.Round(time.Second), err)
		} else {
			failures = 0
			log.Printf("refreshed %s in %s", name, time.Since(start).Round(time.Millisecond))
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(next):
		}
	}
}

// retryDelay backs off from one minute, never past the card's own interval.
func retryDelay(failures int, ttl time.Duration) time.Duration {
	d := time.Minute << min(failures-1, 5)
	if d > ttl {
		d = ttl
	}
	if d > 15*time.Minute {
		d = 15 * time.Minute
	}
	return d
}

func (a *app) refreshLangs(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()

	data, err := a.client.Fetch(ctx, a.fetchOptions(true, false))
	if err != nil {
		return err
	}
	svg, res := a.renderLangs(data)
	if len(res.Languages) == 0 {
		// Publishing this would put an empty card in the README.
		return errors.New("no languages after filtering; keeping the previous card")
	}
	reportLangs(res)

	return a.store.Publish(server.CardLangs, svg, langsSummary(res), time.Now().UTC())
}

func (a *app) refreshStreak(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()

	data, err := a.client.Fetch(ctx, a.fetchOptions(false, true))
	if err != nil {
		return err
	}
	if len(data.Calendars) == 0 {
		return errors.New("no contribution calendars returned; keeping the previous card")
	}
	res := stats.Streaks(data, a.cfg.Streak, a.cfg.Location(), time.Now())
	reportStreak(res)

	return a.store.Publish(server.CardStreak, render.Streak(res, a.cfg.Streak),
		streakSummary(res), time.Now().UTC())
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

func (a *app) enabledCards() []string {
	var out []string
	if a.cfg.Languages.Enabled {
		out = append(out, server.CardLangs)
	}
	if a.cfg.Streak.Enabled {
		out = append(out, server.CardStreak)
	}
	return out
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

func langsSummary(res stats.LangResult) map[string]any {
	rows := make([]map[string]any, 0, len(res.Languages))
	for _, l := range res.Languages {
		rows = append(rows, map[string]any{
			"name":    l.Name,
			"percent": l.Percent,
			"bytes":   l.Size,
		})
	}
	out := map[string]any{
		"languages":        rows,
		"repos_fetched":    res.ReposFetched,
		"repos_considered": res.ReposConsidered,
	}
	if len(res.Warnings) > 0 {
		out["config_warnings"] = res.Warnings
	}
	if len(res.Subtractions) > 0 {
		out["path_exclusions"] = res.Subtractions
	}
	if len(res.ArchivedSkipped) > 0 {
		out["archived_skipped"] = res.ArchivedSkipped
	}
	return out
}

func streakSummary(res stats.StreakResult) map[string]any {
	out := map[string]any{
		"total_contributions": res.TotalContributions,
		"current_streak":      res.Current.Length,
		"longest_streak":      res.Longest.Length,
		"days_considered":     res.DaysConsidered,
		"years":               res.Years,
	}
	if !res.FirstContribution.IsZero() {
		out["first_contribution"] = res.FirstContribution.Format("2006-01-02")
	}
	if res.Current.Length > 0 {
		out["current_streak_range"] = res.Current.Start.Format("2006-01-02") + " to " + res.Current.End.Format("2006-01-02")
	}
	if res.Longest.Length > 0 {
		out["longest_streak_range"] = res.Longest.Start.Format("2006-01-02") + " to " + res.Longest.End.Format("2006-01-02")
	}
	if len(res.Warnings) > 0 {
		out["config_warnings"] = res.Warnings
	}
	return out
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
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("encode dump: %w", err)
	}
	raw = append(raw, '\n')
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
