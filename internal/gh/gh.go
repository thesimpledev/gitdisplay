// Package gh fetches the raw data both cards are built from.
//
// The only outbound destination is the GitHub GraphQL endpoint. Everything
// this package returns can be written to disk with -dump and replayed later,
// so the aggregation and rendering code can be tested without a network.
package gh

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

func readFile(path string) ([]byte, error) { return os.ReadFile(path) }

// Data is the complete raw fetch. It is the -dump format and the fixture
// format the tests read.
type Data struct {
	FetchedAt         time.Time  `json:"fetched_at"`
	Username          string     `json:"username"`
	UserCreatedAt     time.Time  `json:"user_created_at"`
	Timezone          string     `json:"timezone"`
	ContributionYears []int      `json:"contribution_years"`
	Repositories      []Repo     `json:"repositories"`
	Calendars         []Calendar `json:"calendars"`

	// Trees holds the files matched by the configured path exclusions, one
	// entry per repository that has any. Empty when no path exclusions are
	// configured.
	Trees []RepoTree `json:"trees,omitempty"`
}

// RepoTree is the set of files inside one repository that fall under a
// configured path exclusion.
type RepoTree struct {
	Repo string `json:"repo"`
	// Truncated is set when git's tree API could not return the whole tree.
	// Subtractions from a truncated tree are incomplete.
	Truncated bool       `json:"truncated"`
	Prefixes  []string   `json:"prefixes"`
	Files     []TreeFile `json:"files"`
}

// TreeFile is one blob under an excluded prefix.
type TreeFile struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
	// Language is filled in when it was resolved by reading the blob, which
	// currently only happens for the .php / Hack distinction.
	Language string `json:"language,omitempty"`
}

// Repo is one repository and its language byte counts.
type Repo struct {
	Name string `json:"name"`
	// Owner is the account the repository belongs to, and NameWithOwner is
	// the "owner/name" form. With organizations in play a bare name is no
	// longer unique.
	Owner         string `json:"owner"`
	NameWithOwner string `json:"name_with_owner"`
	// Private mirrors GitHub's isPrivate.
	Private bool `json:"private"`
	// Archived mirrors GitHub's isArchived. Archiving a repository is a way
	// to retire it from the card without adding it to an exclusion list.
	Archived  bool   `json:"archived"`
	Languages []Lang `json:"languages"`
}

// Lang is one language within one repository.
type Lang struct {
	Name  string `json:"name"`
	Color string `json:"color"`
	Size  int64  `json:"size"`
}

// Calendar is one year of contribution days as GitHub returned it.
//
// Days are stored exactly as fetched, including the days GitHub pads onto
// the first and last weeks. Those padding days fall outside the requested
// year and overlap the neighbouring year's calendar, so callers must
// deduplicate by date before summing.
type Calendar struct {
	Year int   `json:"year"`
	Days []Day `json:"days"`
}

// Day is a single date and its contribution count.
type Day struct {
	Date  string `json:"date"` // YYYY-MM-DD
	Count int    `json:"count"`
}

// Client talks to the GitHub GraphQL API.
type Client struct {
	Endpoint   string
	Token      string
	UserAgent  string
	MaxRetries int
	HTTP       *http.Client
}

// New builds a client. The http.Client is given an explicit transport with a
// small connection pool; this runs on a 512 MB box and only ever talks to one
// host.
func New(endpoint, token, userAgent string, timeout time.Duration, maxRetries int) *Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = 4
	transport.MaxIdleConnsPerHost = 4
	transport.MaxConnsPerHost = 4
	transport.IdleConnTimeout = 90 * time.Second
	transport.ForceAttemptHTTP2 = true

	return &Client{
		Endpoint:   endpoint,
		Token:      token,
		UserAgent:  userAgent,
		MaxRetries: maxRetries,
		HTTP:       &http.Client{Timeout: timeout, Transport: transport},
	}
}

type graphQLRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables,omitempty"`
}

type graphQLError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

type graphQLResponse struct {
	Data   json.RawMessage `json:"data"`
	Errors []graphQLError  `json:"errors"`
}

// retryableError marks a failure worth trying again.
type retryableError struct {
	err   error
	after time.Duration
}

func (r *retryableError) Error() string { return r.err.Error() }
func (r *retryableError) Unwrap() error { return r.err }

// query runs one GraphQL request and unmarshals data into out.
func (c *Client) query(ctx context.Context, q string, vars map[string]any, out any) error {
	body, err := json.Marshal(graphQLRequest{Query: q, Variables: vars})
	if err != nil {
		return fmt.Errorf("encode request: %w", err)
	}

	var lastErr error
	for attempt := 0; attempt <= c.MaxRetries; attempt++ {
		if attempt > 0 {
			delay := backoff(attempt)
			var re *retryableError
			if errors.As(lastErr, &re) && re.after > 0 {
				delay = re.after
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
		}

		err := c.do(ctx, body, out)
		if err == nil {
			return nil
		}
		lastErr = err

		var re *retryableError
		if !errors.As(err, &re) {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return fmt.Errorf("after %d attempts: %w", c.MaxRetries+1, lastErr)
}

func (c *Client) do(ctx context.Context, body []byte, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.UserAgent)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return &retryableError{err: fmt.Errorf("request: %w", err)}
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
	}()

	// Cap the read. A runaway response must not take the box down.
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return &retryableError{err: fmt.Errorf("read response: %w", err)}
	}

	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		// 403 with a rate-limit reset is retryable; a bad token is not.
		if wait, ok := rateLimitWait(resp); ok {
			return &retryableError{
				err:   fmt.Errorf("rate limited (HTTP %d)", resp.StatusCode),
				after: wait,
			}
		}
		return fmt.Errorf("github rejected the token (HTTP %d): %s",
			resp.StatusCode, firstLine(payload))
	case resp.StatusCode == http.StatusTooManyRequests:
		wait, _ := rateLimitWait(resp)
		return &retryableError{err: fmt.Errorf("HTTP 429"), after: wait}
	case resp.StatusCode >= 500:
		return &retryableError{err: fmt.Errorf("HTTP %d: %s", resp.StatusCode, firstLine(payload))}
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, firstLine(payload))
	}

	var gr graphQLResponse
	if err := json.Unmarshal(payload, &gr); err != nil {
		return &retryableError{err: fmt.Errorf("decode response: %w", err)}
	}
	if len(gr.Errors) > 0 {
		msgs := make([]string, 0, len(gr.Errors))
		retryable := false
		for _, e := range gr.Errors {
			msgs = append(msgs, e.Message)
			// GitHub returns these transiently under load.
			if e.Type == "RATE_LIMITED" || strings.Contains(e.Message, "secondary rate limit") {
				retryable = true
			}
		}
		err := fmt.Errorf("graphql: %s", strings.Join(msgs, "; "))
		if retryable {
			return &retryableError{err: err, after: 60 * time.Second}
		}
		return err
	}
	if len(gr.Data) == 0 {
		return &retryableError{err: fmt.Errorf("graphql returned no data")}
	}
	if err := json.Unmarshal(gr.Data, out); err != nil {
		return fmt.Errorf("decode data: %w", err)
	}
	return nil
}

func rateLimitWait(resp *http.Response) (time.Duration, bool) {
	if v := resp.Header.Get("Retry-After"); v != "" {
		if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
			return time.Duration(secs) * time.Second, true
		}
	}
	if resp.Header.Get("X-RateLimit-Remaining") == "0" {
		if v := resp.Header.Get("X-RateLimit-Reset"); v != "" {
			if unix, err := strconv.ParseInt(v, 10, 64); err == nil {
				wait := time.Until(time.Unix(unix, 0))
				if wait < 0 {
					wait = 0
				}
				// Do not park the refresher for an hour; give up and let the
				// last good render keep serving.
				if wait > 5*time.Minute {
					return 0, false
				}
				return wait + time.Second, true
			}
		}
	}
	return 0, false
}

func backoff(attempt int) time.Duration {
	d := time.Duration(1<<uint(attempt-1)) * time.Second
	if d > 30*time.Second {
		d = 30 * time.Second
	}
	return d
}

func firstLine(b []byte) string {
	s := strings.TrimSpace(string(b))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 300 {
		s = s[:300] + "..."
	}
	return s
}

// ---------------------------------------------------------------------------
// Queries
// ---------------------------------------------------------------------------

const profileQuery = `
query($login: String!) {
  user(login: $login) {
    createdAt
    contributionsCollection {
      contributionYears
    }
  }
}`

// repoQuery pages through repositories.
//
// The original tool asked for repositories(first: 100) and stopped there, so
// every repository past the hundredth was silently dropped. This follows
// pageInfo/endCursor until hasNextPage is false.
//
// ownerAffiliations: OWNER and isFork: false are kept from the original: only
// repositories you own, no forks.
const repoQuery = `
query($login: String!, $cursor: String, $langs: Int!, $affiliations: [RepositoryAffiliation], $privacy: RepositoryPrivacy) {
  user(login: $login) {
    repositories(ownerAffiliations: $affiliations, privacy: $privacy, isFork: false, first: 100, after: $cursor) {
      totalCount
      pageInfo {
        hasNextPage
        endCursor
      }
      nodes {
        name
        nameWithOwner
        isArchived
        isPrivate
        owner { login }
        languages(first: $langs, orderBy: {field: SIZE, direction: DESC}) {
          edges {
            size
            node {
              name
              color
            }
          }
        }
      }
    }
  }
}`

// orgRepoQuery pages through one organization's repositories.
//
// ownerAffiliations: ORGANIZATION_MEMBER on the user connection only returns
// repositories reachable through team membership, which is a much smaller set
// than the organization actually holds. Asking the organization directly is
// the only way to see all of them.
const orgRepoQuery = `
query($org: String!, $cursor: String, $langs: Int!, $privacy: RepositoryPrivacy) {
  organization(login: $org) {
    repositories(privacy: $privacy, isFork: false, first: 100, after: $cursor) {
      totalCount
      pageInfo {
        hasNextPage
        endCursor
      }
      nodes {
        name
        nameWithOwner
        isArchived
        isPrivate
        owner { login }
        languages(first: $langs, orderBy: {field: SIZE, direction: DESC}) {
          edges {
            size
            node {
              name
              color
            }
          }
        }
      }
    }
  }
}`

const calendarQuery = `
query($login: String!, $from: DateTime!, $to: DateTime!) {
  user(login: $login) {
    contributionsCollection(from: $from, to: $to) {
      contributionCalendar {
        weeks {
          contributionDays {
            date
            contributionCount
          }
        }
      }
    }
  }
}`

// Options controls a fetch.
type Options struct {
	Username string
	Location *time.Location

	// LanguagesPerRepo is how many languages to request per repository.
	LanguagesPerRepo int

	// FetchLanguages and FetchStreak select which halves of the data to get.
	FetchLanguages bool
	FetchStreak    bool

	// StartYear, when non-zero, skips contribution years before it.
	StartYear int

	// Now is the reference time, used to clamp the current year's window.
	Now time.Time

	// OwnerAffiliations is the ownerAffiliations argument for the user's own
	// repositories. Defaults to OWNER.
	OwnerAffiliations []string

	// Organizations lists organization logins whose repositories are also
	// pulled in, each queried directly.
	Organizations []string

	// Visibility is "all" or "public". "public" leaves private repositories
	// out entirely.
	Visibility string

	// ExcludePaths maps a repository to the path prefixes to subtract.
	ExcludePaths map[string][]string

	// GlobalExcludePaths applies to every repository that counts.
	GlobalExcludePaths []string

	// ExcludeRepos and ExcludeArchived mirror the language config so that
	// trees are only read for repositories that actually reach the card.
	ExcludeRepos    []string
	ExcludeArchived bool

	// ResolveHackFiles reads .php blobs under an excluded prefix to decide
	// whether GitHub counted them as PHP or as Hack.
	ResolveHackFiles bool
	HackFileLimit    int

	// repos is filled in by Fetch after the repository pass, so fetchTrees
	// knows which repositories exist and who owns them.
	repos []Repo
}

// Fetch collects everything both cards need.
func (c *Client) Fetch(ctx context.Context, opt Options) (*Data, error) {
	if opt.Location == nil {
		return nil, fmt.Errorf("fetch: location is required")
	}
	now := opt.Now
	if now.IsZero() {
		now = time.Now()
	}
	now = now.In(opt.Location)

	data := &Data{
		FetchedAt: now.UTC(),
		Username:  opt.Username,
		Timezone:  opt.Location.String(),
	}

	var profile struct {
		User struct {
			CreatedAt               time.Time `json:"createdAt"`
			ContributionsCollection struct {
				ContributionYears []int `json:"contributionYears"`
			} `json:"contributionsCollection"`
		} `json:"user"`
	}
	if err := c.query(ctx, profileQuery, map[string]any{"login": opt.Username}, &profile); err != nil {
		return nil, fmt.Errorf("profile: %w", err)
	}
	data.UserCreatedAt = profile.User.CreatedAt

	// Use the full contributionYears list GitHub returns.
	//
	// The original clamped the first year to the account creation year unless
	// it was before 2005, which discards contributions imported from history
	// that predates the account. GitHub already tells us exactly which years
	// have contributions; that list is the authority.
	years := append([]int(nil), profile.User.ContributionsCollection.ContributionYears...)
	sortInts(years)
	if opt.StartYear > 0 {
		kept := years[:0]
		for _, y := range years {
			if y >= opt.StartYear {
				kept = append(kept, y)
			}
		}
		years = kept
	}
	data.ContributionYears = years

	if opt.FetchLanguages {
		repos, err := c.fetchRepos(ctx, opt)
		if err != nil {
			return nil, fmt.Errorf("repositories: %w", err)
		}
		data.Repositories = repos

		if len(opt.ExcludePaths) > 0 || len(opt.GlobalExcludePaths) > 0 {
			opt.repos = repos
			trees, err := c.fetchTrees(ctx, opt)
			if err != nil {
				return nil, fmt.Errorf("path exclusions: %w", err)
			}
			data.Trees = trees
		}
	}

	if opt.FetchStreak {
		cals, err := c.fetchCalendars(ctx, opt, years, now)
		if err != nil {
			return nil, fmt.Errorf("calendars: %w", err)
		}
		data.Calendars = cals
	}

	return data, nil
}

// repoNode is the shape both repository queries return.
type repoNode struct {
	Name          string `json:"name"`
	NameWithOwner string `json:"nameWithOwner"`
	IsArchived    bool   `json:"isArchived"`
	IsPrivate     bool   `json:"isPrivate"`
	Owner         struct {
		Login string `json:"login"`
	} `json:"owner"`
	Languages struct {
		Edges []struct {
			Size int64 `json:"size"`
			Node struct {
				Name  string `json:"name"`
				Color string `json:"color"`
			} `json:"node"`
		} `json:"edges"`
	} `json:"languages"`
}

type repoPage struct {
	TotalCount int `json:"totalCount"`
	PageInfo   struct {
		HasNextPage bool   `json:"hasNextPage"`
		EndCursor   string `json:"endCursor"`
	} `json:"pageInfo"`
	Nodes []repoNode `json:"nodes"`
}

func toRepo(n repoNode) Repo {
	repo := Repo{
		Name:          n.Name,
		Owner:         n.Owner.Login,
		NameWithOwner: n.NameWithOwner,
		Archived:      n.IsArchived,
		Private:       n.IsPrivate,
	}
	for _, e := range n.Languages.Edges {
		repo.Languages = append(repo.Languages, Lang{
			Name:  e.Node.Name,
			Color: e.Node.Color,
			Size:  e.Size,
		})
	}
	return repo
}

// fetchRepos reads the user's own repositories and then each configured
// organization, deduplicating by owner/name.
func (c *Client) fetchRepos(ctx context.Context, opt Options) ([]Repo, error) {
	langs := opt.LanguagesPerRepo
	if langs < 1 {
		langs = 50
	}

	var privacy any
	if strings.EqualFold(opt.Visibility, "public") {
		privacy = "PUBLIC"
	}

	affiliations := opt.OwnerAffiliations
	if len(affiliations) == 0 {
		affiliations = []string{"OWNER"}
	}

	var out []Repo
	seen := map[string]bool{}

	add := func(nodes []repoNode) {
		for _, n := range nodes {
			key := n.NameWithOwner
			if key == "" {
				key = n.Owner.Login + "/" + n.Name
			}
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, toRepo(n))
		}
	}

	page := func(query string, vars map[string]any, pick func(json.RawMessage) (repoPage, error)) error {
		// A hard page ceiling so a pathological response cannot loop forever.
		const maxPages = 100
		var cursor *string
		for i := 0; i < maxPages; i++ {
			v := make(map[string]any, len(vars)+1)
			for k, val := range vars {
				v[k] = val
			}
			if cursor != nil {
				v["cursor"] = *cursor
			}
			var raw json.RawMessage
			if err := c.query(ctx, query, v, &raw); err != nil {
				return err
			}
			p, err := pick(raw)
			if err != nil {
				return err
			}
			add(p.Nodes)
			if !p.PageInfo.HasNextPage || p.PageInfo.EndCursor == "" {
				return nil
			}
			end := p.PageInfo.EndCursor
			cursor = &end
		}
		return nil
	}

	userVars := map[string]any{
		"login": opt.Username, "langs": langs, "affiliations": affiliations,
	}
	if privacy != nil {
		userVars["privacy"] = privacy
	}
	err := page(repoQuery, userVars, func(raw json.RawMessage) (repoPage, error) {
		var r struct {
			User struct {
				Repositories repoPage `json:"repositories"`
			} `json:"user"`
		}
		err := json.Unmarshal(raw, &r)
		return r.User.Repositories, err
	})
	if err != nil {
		return nil, err
	}

	for _, org := range opt.Organizations {
		if strings.TrimSpace(org) == "" {
			continue
		}
		orgVars := map[string]any{"org": org, "langs": langs}
		if privacy != nil {
			orgVars["privacy"] = privacy
		}
		err := page(orgRepoQuery, orgVars, func(raw json.RawMessage) (repoPage, error) {
			var r struct {
				Organization struct {
					Repositories repoPage `json:"repositories"`
				} `json:"organization"`
			}
			err := json.Unmarshal(raw, &r)
			return r.Organization.Repositories, err
		})
		if err != nil {
			return nil, fmt.Errorf("organization %s: %w", org, err)
		}
	}

	return out, nil
}

func (c *Client) fetchCalendars(ctx context.Context, opt Options, years []int, now time.Time) ([]Calendar, error) {
	out := make([]Calendar, 0, len(years))
	for _, year := range years {
		from := time.Date(year, time.January, 1, 0, 0, 0, 0, opt.Location)
		to := time.Date(year, time.December, 31, 23, 59, 59, 0, opt.Location)
		// GitHub rejects a window that runs past the present.
		if to.After(now) {
			to = now
		}
		if from.After(now) {
			continue
		}

		var resp struct {
			User struct {
				ContributionsCollection struct {
					ContributionCalendar struct {
						Weeks []struct {
							ContributionDays []struct {
								Date              string `json:"date"`
								ContributionCount int    `json:"contributionCount"`
							} `json:"contributionDays"`
						} `json:"weeks"`
					} `json:"contributionCalendar"`
				} `json:"contributionsCollection"`
			} `json:"user"`
		}

		vars := map[string]any{
			"login": opt.Username,
			"from":  from.Format(time.RFC3339),
			"to":    to.Format(time.RFC3339),
		}
		if err := c.query(ctx, calendarQuery, vars, &resp); err != nil {
			return nil, fmt.Errorf("year %d: %w", year, err)
		}

		cal := Calendar{Year: year}
		for _, w := range resp.User.ContributionsCollection.ContributionCalendar.Weeks {
			for _, d := range w.ContributionDays {
				cal.Days = append(cal.Days, Day{Date: d.Date, Count: d.ContributionCount})
			}
		}
		out = append(out, cal)
	}
	return out, nil
}

// fetchTrees reads the git tree of each repository that has a path exclusion
// and keeps the blobs under the configured prefixes.
//
// This uses the REST git-trees endpoint rather than GraphQL. GraphQL can only
// walk a tree one nesting level per query, which would mean an unbounded
// number of round trips; REST returns the whole tree in one call. It is the
// same host and the same token, so the egress surface is unchanged: still
// nothing but api.github.com.
func (c *Client) fetchTrees(ctx context.Context, opt Options) ([]RepoTree, error) {
	global := normalizePrefixes(opt.GlobalExcludePaths)

	// Work out which repositories to read. Reading a tree costs an API call,
	// so repositories that never reach the card are skipped.
	skip := make(map[string]bool, len(opt.ExcludeRepos))
	for _, n := range opt.ExcludeRepos {
		skip[n] = true
	}

	type target struct {
		repo     Repo
		prefixes []string
	}
	var targets []target
	seen := map[string]bool{}

	if len(global) > 0 {
		for _, r := range opt.repos {
			if skip[r.Name] || skip[r.NameWithOwner] {
				continue
			}
			if opt.ExcludeArchived && r.Archived {
				continue
			}
			targets = append(targets, target{repo: r, prefixes: global})
			seen[r.Name] = true
		}
	}

	for _, name := range sortedKeysOf(opt.ExcludePaths) {
		prefixes := normalizePrefixes(opt.ExcludePaths[name])
		if len(prefixes) == 0 {
			continue
		}
		if seen[name] {
			// Already queued for the global sweep; merge the extra prefixes.
			for i := range targets {
				if targets[i].repo.Name == name {
					targets[i].prefixes = mergePrefixes(targets[i].prefixes, prefixes)
					break
				}
			}
			continue
		}
		repo := Repo{Name: name, Owner: opt.Username}
		for _, r := range opt.repos {
			if r.Name == name || r.NameWithOwner == name {
				repo = r
				break
			}
		}
		targets = append(targets, target{repo: repo, prefixes: prefixes})
	}

	hackBudget := opt.HackFileLimit
	if hackBudget < 0 {
		hackBudget = 0
	}

	out := make([]RepoTree, 0, len(targets))
	for _, tgt := range targets {
		owner := tgt.repo.Owner
		if owner == "" {
			owner = opt.Username
		}

		var resp struct {
			Tree []struct {
				Path string `json:"path"`
				Type string `json:"type"`
				Size int64  `json:"size"`
				SHA  string `json:"sha"`
			} `json:"tree"`
			Truncated bool `json:"truncated"`
		}
		path := fmt.Sprintf("/repos/%s/%s/git/trees/HEAD?recursive=1",
			url.PathEscape(owner), url.PathEscape(tgt.repo.Name))
		if err := c.rest(ctx, path, &resp); err != nil {
			// An empty repository has no HEAD, and a repository named in the
			// config that cannot be read is a configuration problem rather
			// than a reason to fail the whole refresh. Record it as empty;
			// validation reports the mismatch.
			var nf *notFoundError
			if errors.As(err, &nf) {
				out = append(out, RepoTree{Repo: tgt.repo.Name, Prefixes: tgt.prefixes})
				continue
			}
			return nil, fmt.Errorf("repo %s: %w", tgt.repo.Name, err)
		}

		rt := RepoTree{Repo: tgt.repo.Name, Truncated: resp.Truncated, Prefixes: tgt.prefixes}
		for _, e := range resp.Tree {
			if e.Type != "blob" || !matchesPrefix(e.Path, tgt.prefixes) {
				continue
			}
			f := TreeFile{Path: e.Path, Size: e.Size}

			// GitHub labels a .php file with no "<?php" opening tag as Hack.
			// Read the blob so the right language gets the subtraction.
			if opt.ResolveHackFiles && strings.EqualFold(pathExt(e.Path), ".php") && hackBudget > 0 {
				hackBudget--
				if isPHP, err := c.blobHasPHPTag(ctx, owner, tgt.repo.Name, e.SHA); err == nil {
					if isPHP {
						f.Language = "PHP"
					} else {
						f.Language = "Hack"
					}
				}
			}
			rt.Files = append(rt.Files, f)
		}
		// Only keep trees that matched something; an empty one is noise.
		if len(rt.Files) > 0 || len(opt.ExcludePaths[tgt.repo.Name]) > 0 {
			out = append(out, rt)
		}
	}
	return out, nil
}

func mergePrefixes(a, b []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range append(append([]string{}, a...), b...) {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	sortStrings(out)
	return out
}

func sortedKeysOf(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sortStrings(out)
	return out
}

// blobHasPHPTag reports whether a blob contains a "<?php" opening tag.
func (c *Client) blobHasPHPTag(ctx context.Context, owner, repo, sha string) (bool, error) {
	var resp struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
	}
	path := fmt.Sprintf("/repos/%s/%s/git/blobs/%s",
		url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(sha))
	if err := c.rest(ctx, path, &resp); err != nil {
		return false, err
	}
	if resp.Encoding != "base64" {
		return strings.Contains(resp.Content, "<?php"), nil
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(resp.Content, "\n", ""))
	if err != nil {
		return false, fmt.Errorf("decode blob: %w", err)
	}
	return bytes.Contains(decoded, []byte("<?php")), nil
}

type notFoundError struct{ msg string }

func (n *notFoundError) Error() string { return n.msg }

// rest performs a GET against the REST API on the same host as the GraphQL
// endpoint, reusing the retry and rate-limit handling.
func (c *Client) rest(ctx context.Context, path string, out any) error {
	base, err := url.Parse(c.Endpoint)
	if err != nil {
		return fmt.Errorf("bad endpoint %q: %w", c.Endpoint, err)
	}
	target := (&url.URL{Scheme: base.Scheme, Host: base.Host}).String() + path

	var lastErr error
	for attempt := 0; attempt <= c.MaxRetries; attempt++ {
		if attempt > 0 {
			delay := backoff(attempt)
			var re *retryableError
			if errors.As(lastErr, &re) && re.after > 0 {
				delay = re.after
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
		}

		err := c.doREST(ctx, target, out)
		if err == nil {
			return nil
		}
		lastErr = err
		var re *retryableError
		if !errors.As(err, &re) {
			return err
		}
	}
	return fmt.Errorf("after %d attempts: %w", c.MaxRetries+1, lastErr)
}

func (c *Client) doREST(ctx context.Context, target string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", c.UserAgent)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return &retryableError{err: fmt.Errorf("request: %w", err)}
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
	}()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return &retryableError{err: fmt.Errorf("read response: %w", err)}
	}

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return &notFoundError{msg: fmt.Sprintf("not found: %s", firstLine(payload))}
	case resp.StatusCode == http.StatusConflict:
		// A repository with no commits has no tree to walk. That is a normal
		// state, not a failure worth stopping the refresh over.
		return &notFoundError{msg: fmt.Sprintf("nothing to read: %s", firstLine(payload))}
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		if wait, ok := rateLimitWait(resp); ok {
			return &retryableError{err: fmt.Errorf("rate limited (HTTP %d)", resp.StatusCode), after: wait}
		}
		return fmt.Errorf("github rejected the token (HTTP %d): %s", resp.StatusCode, firstLine(payload))
	case resp.StatusCode == http.StatusTooManyRequests:
		wait, _ := rateLimitWait(resp)
		return &retryableError{err: fmt.Errorf("HTTP 429"), after: wait}
	case resp.StatusCode >= 500:
		return &retryableError{err: fmt.Errorf("HTTP %d: %s", resp.StatusCode, firstLine(payload))}
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, firstLine(payload))
	}

	if err := json.Unmarshal(payload, out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

// normalizePrefixes trims and canonicalises path prefixes. A prefix that
// names a directory gets a trailing slash so "themes/vdg" cannot match
// "themes/vdgx/style.css".
func normalizePrefixes(in []string) []string {
	out := make([]string, 0, len(in))
	for _, p := range in {
		p = strings.TrimSpace(p)
		p = strings.TrimPrefix(p, "./")
		p = strings.TrimPrefix(p, "/")
		if p == "" {
			continue
		}
		out = append(out, p)
	}
	sortStrings(out)
	return out
}

// matchesPrefix reports whether a file path sits under one of the prefixes.
// A prefix matches either the exact file or a directory containing it.
func matchesPrefix(path string, prefixes []string) bool {
	for _, p := range prefixes {
		if path == p {
			return true
		}
		dir := strings.TrimSuffix(p, "/") + "/"
		if strings.HasPrefix(path, dir) {
			return true
		}
	}
	return false
}

func pathExt(p string) string {
	if i := strings.LastIndexByte(p, '.'); i >= 0 && !strings.ContainsAny(p[i:], "/") {
		return p[i:]
	}
	return ""
}

func sortStrings(v []string) {
	for i := 1; i < len(v); i++ {
		for j := i; j > 0 && v[j] < v[j-1]; j-- {
			v[j], v[j-1] = v[j-1], v[j]
		}
	}
}

func sortInts(v []int) {
	for i := 1; i < len(v); i++ {
		for j := i; j > 0 && v[j] < v[j-1]; j-- {
			v[j], v[j-1] = v[j-1], v[j]
		}
	}
}

// LoadDump reads a -dump file back in, for offline rendering and tests.
func LoadDump(path string) (*Data, error) {
	raw, err := readFile(path)
	if err != nil {
		return nil, err
	}
	var d Data
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, fmt.Errorf("parse dump %s: %w", path, err)
	}
	return &d, nil
}
