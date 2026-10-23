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
	"maps"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

func readFile(path string) ([]byte, error) {
	return os.ReadFile(path) // #nosec G304 -- the path is the operator's own -from/-dump CLI argument; reading it is the feature
}

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

	// after waits between retries; nil means time.After. Tests replace it
	// so retry paths run without sleeping.
	after func(time.Duration) <-chan time.Time
}

func (c *Client) wait(d time.Duration) <-chan time.Time {
	if c.after != nil {
		return c.after(d)
	}
	return time.After(d)
}

// New builds a client. The http.Client is given an explicit transport with a
// small connection pool; a run is a handful of calls to one host.
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

	return c.retry(ctx, func() error {
		doErr := c.do(ctx, body, out)
		if _, ok := errors.AsType[*retryableError](doErr); ok && ctx.Err() != nil {
			return ctx.Err()
		}
		return doErr
	})
}

// retry runs attempt until it succeeds, fails with an error that is not
// retryable, or uses up MaxRetries.
func (c *Client) retry(ctx context.Context, attempt func() error) error {
	var lastErr error
	for n := 0; n <= c.MaxRetries; n++ {
		if n > 0 {
			if err := c.pause(ctx, n, lastErr); err != nil {
				return err
			}
		}
		err := attempt()
		if err == nil {
			return nil
		}
		lastErr = err
		if _, ok := errors.AsType[*retryableError](err); !ok {
			return err
		}
	}
	return fmt.Errorf("after %d attempts: %w", c.MaxRetries+1, lastErr)
}

// pause waits before retry number attempt, preferring the delay the server
// asked for over the exponential backoff.
func (c *Client) pause(ctx context.Context, attempt int, lastErr error) error {
	delay := backoff(attempt)
	if re, ok := errors.AsType[*retryableError](lastErr); ok && re.after > 0 {
		delay = re.after
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.wait(delay):
		return nil
	}
}

func (c *Client) do(ctx context.Context, body []byte, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	// Cap the read. A runaway response must not take the box down.
	resp, payload, err := c.send(req, 32<<20)
	if err != nil {
		return err
	}
	if err = statusError(resp, payload); err != nil {
		return err
	}
	return decodeGraphQL(payload, out)
}

// send adds the auth headers, performs the request, and reads at most limit
// bytes of the body. Transport and read failures are retryable.
func (c *Client) send(req *http.Request, limit int64) (*http.Response, []byte, error) {
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("User-Agent", c.UserAgent)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, nil, &retryableError{err: fmt.Errorf("request: %w", err)}
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
	}()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return nil, nil, &retryableError{err: fmt.Errorf("read response: %w", err)}
	}
	return resp, payload, nil
}

// statusError maps a non-200 status to an error, retryable where trying
// again can help. It returns nil for 200.
func statusError(resp *http.Response, payload []byte) error {
	code := resp.StatusCode
	switch {
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		// 403 with a rate-limit reset is retryable; a bad token is not.
		if wait, ok := rateLimitWait(resp); ok {
			return &retryableError{err: fmt.Errorf("rate limited (HTTP %d)", code), after: wait}
		}
		return fmt.Errorf("github rejected the token (HTTP %d): %s", code, firstLine(payload))
	case code == http.StatusTooManyRequests:
		wait, _ := rateLimitWait(resp)
		return &retryableError{err: fmt.Errorf("HTTP 429"), after: wait}
	case code >= 500:
		return &retryableError{err: fmt.Errorf("HTTP %d: %s", code, firstLine(payload))}
	case code != http.StatusOK:
		return fmt.Errorf("HTTP %d: %s", code, firstLine(payload))
	}
	return nil
}

// decodeGraphQL unwraps a GraphQL response envelope into out.
func decodeGraphQL(payload []byte, out any) error {
	var gr graphQLResponse
	if err := json.Unmarshal(payload, &gr); err != nil {
		return &retryableError{err: fmt.Errorf("decode response: %w", err)}
	}
	if len(gr.Errors) > 0 {
		return graphQLErrors(gr.Errors)
	}
	if len(gr.Data) == 0 {
		return &retryableError{err: fmt.Errorf("graphql returned no data")}
	}
	if err := json.Unmarshal(gr.Data, out); err != nil {
		return fmt.Errorf("decode data: %w", err)
	}
	return nil
}

func graphQLErrors(errs []graphQLError) error {
	msgs := make([]string, 0, len(errs))
	retryable := false
	for _, e := range errs {
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

func rateLimitWait(resp *http.Response) (time.Duration, bool) {
	if wait, ok := retryAfter(resp.Header); ok {
		return wait, true
	}
	return resetWait(resp.Header)
}

func retryAfter(h http.Header) (time.Duration, bool) {
	secs, err := strconv.Atoi(h.Get("Retry-After"))
	if err != nil || secs < 0 {
		return 0, false
	}
	return time.Duration(secs) * time.Second, true
}

func resetWait(h http.Header) (time.Duration, bool) {
	if h.Get("X-RateLimit-Remaining") != "0" {
		return 0, false
	}
	unix, err := strconv.ParseInt(h.Get("X-RateLimit-Reset"), 10, 64)
	if err != nil {
		return 0, false
	}
	wait := max(time.Until(time.Unix(unix, 0)), 0)
	// Do not park the refresher for an hour; give up and let the last good
	// render keep serving.
	if wait > 5*time.Minute {
		return 0, false
	}
	return wait + time.Second, true
}

func backoff(attempt int) time.Duration {
	// Bound the shift so the doubling can never overflow; past 5 the cap
	// below decides anyway.
	shift := min(max(attempt-1, 0), 5)
	d := min(time.Duration(1<<shift)*time.Second, 30*time.Second)
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

	if err := c.fetchProfile(ctx, opt, data); err != nil {
		return nil, err
	}
	if opt.FetchLanguages {
		if err := c.fetchLanguageData(ctx, opt, data); err != nil {
			return nil, err
		}
	}
	if opt.FetchStreak {
		cals, err := c.fetchCalendars(ctx, opt, data.ContributionYears, now)
		if err != nil {
			return nil, fmt.Errorf("calendars: %w", err)
		}
		data.Calendars = cals
	}
	return data, nil
}

// fetchProfile fills in the account creation time and contribution years.
func (c *Client) fetchProfile(ctx context.Context, opt Options, data *Data) error {
	var profile struct {
		User struct {
			CreatedAt               time.Time `json:"createdAt"`
			ContributionsCollection struct {
				ContributionYears []int `json:"contributionYears"`
			} `json:"contributionsCollection"`
		} `json:"user"`
	}
	if err := c.query(ctx, profileQuery, map[string]any{"login": opt.Username}, &profile); err != nil {
		return fmt.Errorf("profile: %w", err)
	}
	data.UserCreatedAt = profile.User.CreatedAt

	// Use the full contributionYears list GitHub returns.
	//
	// The original clamped the first year to the account creation year unless
	// it was before 2005, which discards contributions imported from history
	// that predates the account. GitHub already tells us exactly which years
	// have contributions; that list is the authority.
	data.ContributionYears = yearsFrom(profile.User.ContributionsCollection.ContributionYears, opt.StartYear)
	return nil
}

// yearsFrom returns a sorted copy of years, without those before start when
// start is set.
func yearsFrom(years []int, start int) []int {
	out := append([]int(nil), years...)
	sortInts(out)
	if start <= 0 {
		return out
	}
	kept := out[:0]
	for _, y := range out {
		if y >= start {
			kept = append(kept, y)
		}
	}
	return kept
}

// fetchLanguageData fills in the repositories and, when path exclusions are
// configured, the matching parts of their trees.
func (c *Client) fetchLanguageData(ctx context.Context, opt Options, data *Data) error {
	repos, err := c.fetchRepos(ctx, opt)
	if err != nil {
		return fmt.Errorf("repositories: %w", err)
	}
	data.Repositories = repos

	if len(opt.ExcludePaths) == 0 && len(opt.GlobalExcludePaths) == 0 {
		return nil
	}
	opt.repos = repos
	trees, err := c.fetchTrees(ctx, opt)
	if err != nil {
		return fmt.Errorf("path exclusions: %w", err)
	}
	data.Trees = trees
	return nil
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
	affiliations := opt.OwnerAffiliations
	if len(affiliations) == 0 {
		affiliations = []string{"OWNER"}
	}

	set := &repoSet{seen: map[string]bool{}}
	userVars := withPrivacy(map[string]any{
		"login": opt.Username, "langs": langs, "affiliations": affiliations,
	}, opt.Visibility)
	if err := c.pageRepos(ctx, repoQuery, userVars, userRepoPage, set); err != nil {
		return nil, err
	}

	for _, org := range opt.Organizations {
		if strings.TrimSpace(org) == "" {
			continue
		}
		orgVars := withPrivacy(map[string]any{"org": org, "langs": langs}, opt.Visibility)
		if err := c.pageRepos(ctx, orgRepoQuery, orgVars, orgRepoPage, set); err != nil {
			return nil, fmt.Errorf("organization %s: %w", org, err)
		}
	}
	return set.repos, nil
}

// withPrivacy adds the PUBLIC privacy filter when visibility is "public".
func withPrivacy(vars map[string]any, visibility string) map[string]any {
	if strings.EqualFold(visibility, "public") {
		vars["privacy"] = "PUBLIC"
	}
	return vars
}

// repoSet collects repositories in arrival order, deduplicated by owner/name.
type repoSet struct {
	seen  map[string]bool
	repos []Repo
}

func (s *repoSet) add(nodes []repoNode) {
	for _, n := range nodes {
		key := n.NameWithOwner
		if key == "" {
			key = n.Owner.Login + "/" + n.Name
		}
		if s.seen[key] {
			continue
		}
		s.seen[key] = true
		s.repos = append(s.repos, toRepo(n))
	}
}

// pageRepos follows one repository connection page by page into set.
func (c *Client) pageRepos(ctx context.Context, query string, vars map[string]any,
	pick func(json.RawMessage) (repoPage, error), set *repoSet) error {
	// A hard page ceiling so a pathological response cannot loop forever.
	const maxPages = 100
	cursor := ""
	for range maxPages {
		v := make(map[string]any, len(vars)+1)
		maps.Copy(v, vars)
		if cursor != "" {
			v["cursor"] = cursor
		}
		var raw json.RawMessage
		if err := c.query(ctx, query, v, &raw); err != nil {
			return err
		}
		p, err := pick(raw)
		if err != nil {
			return err
		}
		set.add(p.Nodes)
		if !p.PageInfo.HasNextPage || p.PageInfo.EndCursor == "" {
			return nil
		}
		cursor = p.PageInfo.EndCursor
	}
	return nil
}

func userRepoPage(raw json.RawMessage) (repoPage, error) {
	var r struct {
		User struct {
			Repositories repoPage `json:"repositories"`
		} `json:"user"`
	}
	err := json.Unmarshal(raw, &r)
	return r.User.Repositories, err
}

func orgRepoPage(raw json.RawMessage) (repoPage, error) {
	var r struct {
		Organization struct {
			Repositories repoPage `json:"repositories"`
		} `json:"organization"`
	}
	err := json.Unmarshal(raw, &r)
	return r.Organization.Repositories, err
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
	targets := treeTargets(opt)
	r := &treeReader{c: c, opt: opt, hackBudget: max(opt.HackFileLimit, 0)}
	out := make([]RepoTree, 0, len(targets))
	for _, tgt := range targets {
		rt, keep, err := r.read(ctx, tgt)
		if err != nil {
			return nil, fmt.Errorf("repo %s: %w", tgt.repo.Name, err)
		}
		if keep {
			out = append(out, rt)
		}
	}
	return out, nil
}

// treeTarget is one repository whose tree is read, and the prefixes to keep.
type treeTarget struct {
	repo     Repo
	prefixes []string
}

// treeTargets works out which repositories to read: every repository that
// reaches the card when global prefixes are set, then each repository with
// its own prefixes. A repository in both is read once with the prefixes
// merged.
func treeTargets(opt Options) []treeTarget {
	targets := globalTargets(opt)
	global := len(targets)
	for _, name := range sortedKeysOf(opt.ExcludePaths) {
		prefixes := normalizePrefixes(opt.ExcludePaths[name])
		if len(prefixes) == 0 {
			continue
		}
		if i := indexByName(targets[:global], name); i >= 0 {
			// Already queued for the global sweep; merge the extra prefixes.
			targets[i].prefixes = mergePrefixes(targets[i].prefixes, prefixes)
			continue
		}
		targets = append(targets, treeTarget{repo: findRepo(opt, name), prefixes: prefixes})
	}
	return targets
}

// globalTargets queues every repository that reaches the card for the global
// prefixes. Reading a tree costs an API call, so repositories that never
// reach the card are skipped.
func globalTargets(opt Options) []treeTarget {
	global := normalizePrefixes(opt.GlobalExcludePaths)
	if len(global) == 0 {
		return nil
	}
	skip := make(map[string]bool, len(opt.ExcludeRepos))
	for _, n := range opt.ExcludeRepos {
		skip[n] = true
	}
	var targets []treeTarget
	for _, r := range opt.repos {
		if skip[r.Name] || skip[r.NameWithOwner] || (opt.ExcludeArchived && r.Archived) {
			continue
		}
		targets = append(targets, treeTarget{repo: r, prefixes: global})
	}
	return targets
}

func indexByName(targets []treeTarget, name string) int {
	for i := range targets {
		if targets[i].repo.Name == name {
			return i
		}
	}
	return -1
}

// findRepo resolves a config key, bare or owner/name, to a fetched
// repository. An unknown key is assumed to be one of the user's own.
func findRepo(opt Options, name string) Repo {
	for _, r := range opt.repos {
		if r.Name == name || r.NameWithOwner == name {
			return r
		}
	}
	return Repo{Name: name, Owner: opt.Username}
}

// treeEntry is one entry of a recursive git tree listing.
type treeEntry struct {
	Path string `json:"path"`
	Type string `json:"type"`
	Size int64  `json:"size"`
	SHA  string `json:"sha"`
}

type treeListing struct {
	Tree      []treeEntry `json:"tree"`
	Truncated bool        `json:"truncated"`
}

// treeReader reads trees for one fetch. hackBudget is shared across every
// repository so HackFileLimit caps the whole run.
type treeReader struct {
	c          *Client
	opt        Options
	hackBudget int
}

// read returns the files of one repository under its prefixes, and whether
// the result is worth keeping.
func (r *treeReader) read(ctx context.Context, tgt treeTarget) (RepoTree, bool, error) {
	owner := tgt.repo.Owner
	if owner == "" {
		owner = r.opt.Username
	}
	listing, err := r.c.treeListing(ctx, owner, tgt.repo.Name)
	if err != nil {
		// An empty repository has no HEAD, and a repository named in the
		// config that cannot be read is a configuration problem rather than
		// a reason to fail the whole refresh. Record it as empty; validation
		// reports the mismatch.
		if _, ok := errors.AsType[*notFoundError](err); ok {
			return RepoTree{Repo: tgt.repo.Name, Prefixes: tgt.prefixes}, true, nil
		}
		return RepoTree{}, false, err
	}

	rt := RepoTree{Repo: tgt.repo.Name, Truncated: listing.Truncated, Prefixes: tgt.prefixes}
	for _, e := range listing.Tree {
		if e.Type != "blob" || !matchesPrefix(e.Path, tgt.prefixes) {
			continue
		}
		rt.Files = append(rt.Files, TreeFile{
			Path: e.Path, Size: e.Size, Language: r.resolveLanguage(ctx, owner, tgt.repo.Name, e),
		})
	}
	// Only keep trees that matched something; an empty one is noise.
	keep := len(rt.Files) > 0 || len(r.opt.ExcludePaths[tgt.repo.Name]) > 0
	return rt, keep, nil
}

// resolveLanguage settles the .php / Hack question for one file, or returns
// "" to leave the language to the extension table.
//
// GitHub labels a .php file with no "<?php" opening tag as Hack. Read the
// blob so the right language gets the subtraction. A blob that cannot be read
// falls back to the extension, which is the same answer as not checking.
func (r *treeReader) resolveLanguage(ctx context.Context, owner, repo string, e treeEntry) string {
	if !r.opt.ResolveHackFiles || !strings.EqualFold(pathExt(e.Path), ".php") || r.hackBudget <= 0 {
		return ""
	}
	r.hackBudget--
	isPHP, err := r.c.blobHasPHPTag(ctx, owner, repo, e.SHA)
	if err != nil {
		return ""
	}
	if isPHP {
		return "PHP"
	}
	return "Hack"
}

func (c *Client) treeListing(ctx context.Context, owner, repo string) (treeListing, error) {
	var listing treeListing
	path := fmt.Sprintf("/repos/%s/%s/git/trees/HEAD?recursive=1",
		url.PathEscape(owner), url.PathEscape(repo))
	if err := c.rest(ctx, path, &listing); err != nil {
		return treeListing{}, err
	}
	return listing, nil
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

	return c.retry(ctx, func() error { return c.doREST(ctx, target, out) })
}

func (c *Client) doREST(ctx context.Context, target string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, payload, err := c.send(req, 16<<20)
	if err != nil {
		return err
	}

	switch resp.StatusCode {
	case http.StatusNotFound:
		return &notFoundError{msg: fmt.Sprintf("not found: %s", firstLine(payload))}
	case http.StatusConflict:
		// A repository with no commits has no tree to walk. That is a normal
		// state, not a failure worth stopping the refresh over.
		return &notFoundError{msg: fmt.Sprintf("nothing to read: %s", firstLine(payload))}
	}
	if err = statusError(resp, payload); err != nil {
		return err
	}

	if err = json.Unmarshal(payload, out); err != nil {
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
