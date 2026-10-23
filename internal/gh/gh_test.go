package gh

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// gqlCall is one GraphQL request the fake server received.
type gqlCall struct {
	kind string
	vars map[string]any
}

func queryKind(q string) string {
	switch {
	case strings.Contains(q, "createdAt"):
		return "profile"
	case strings.Contains(q, "organization("):
		return "org"
	case strings.Contains(q, "contributionCalendar"):
		return "calendar"
	case strings.Contains(q, "repositories("):
		return "repos"
	}
	return "unknown"
}

// fakeGitHub answers the queries Fetch makes from canned responses.
type fakeGitHub struct {
	mu      sync.Mutex
	calls   []gqlCall
	paths   []string
	graphql func(kind string, vars map[string]any) (int, string)
	rest    map[string]restReply
}

type restReply struct {
	status int
	body   string
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		f.mu.Lock()
		f.paths = append(f.paths, r.URL.RequestURI())
		f.mu.Unlock()
		reply, ok := f.rest[r.URL.RequestURI()]
		if !ok {
			reply = restReply{status: http.StatusNotFound, body: `{"message":"Not Found"}`}
		}
		w.WriteHeader(reply.status)
		_, _ = io.WriteString(w, reply.body)
		return
	}
	var req graphQLRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	kind := queryKind(req.Query)
	f.mu.Lock()
	f.calls = append(f.calls, gqlCall{kind: kind, vars: req.Variables})
	f.mu.Unlock()
	status, body := f.graphql(kind, req.Variables)
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func (f *fakeGitHub) callsOf(kind string) []gqlCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []gqlCall
	for _, c := range f.calls {
		if c.kind == kind {
			out = append(out, c)
		}
	}
	return out
}

func newTestClient(t *testing.T, h http.Handler, retries int) (*Client, *[]time.Duration) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := New(srv.URL+"/graphql", "test-token", "gitdisplay-test", 5*time.Second, retries)
	var mu sync.Mutex
	delays := &[]time.Duration{}
	c.after = func(d time.Duration) <-chan time.Time {
		mu.Lock()
		*delays = append(*delays, d)
		mu.Unlock()
		ch := make(chan time.Time, 1)
		ch <- time.Time{}
		return ch
	}
	return c, delays
}

func data(v string) string { return `{"data":` + v + `}` }

func repoNodeJSON(owner, name, nwo string, archived bool, langs ...string) string {
	edges := make([]string, 0, len(langs))
	for _, l := range langs {
		parts := strings.SplitN(l, ":", 2)
		edges = append(edges, fmt.Sprintf(`{"size":%s,"node":{"name":%q,"color":"#123456"}}`, parts[1], parts[0]))
	}
	return fmt.Sprintf(`{"name":%q,"nameWithOwner":%q,"isArchived":%t,"isPrivate":false,"owner":{"login":%q},`+
		`"languages":{"edges":[%s]}}`, name, nwo, archived, owner, strings.Join(edges, ","))
}

func repoPageJSON(next bool, cursor string, nodes ...string) string {
	return fmt.Sprintf(`{"totalCount":%d,"pageInfo":{"hasNextPage":%t,"endCursor":%q},"nodes":[%s]}`,
		len(nodes), next, cursor, strings.Join(nodes, ","))
}

func treeJSON(truncated bool, entries ...string) string {
	return fmt.Sprintf(`{"tree":[%s],"truncated":%t}`, strings.Join(entries, ","), truncated)
}

func blobEntry(path string, size int, sha string) string {
	return fmt.Sprintf(`{"path":%q,"type":"blob","size":%d,"sha":%q}`, path, size, sha)
}

func fullFetchServer() *fakeGitHub {
	b64 := base64.StdEncoding.EncodeToString([]byte("<html><?php echo 1; ?>"))
	// GitHub wraps base64 content at 60 columns.
	b64 = b64[:10] + "\n" + b64[10:]
	return &fakeGitHub{
		graphql: func(kind string, vars map[string]any) (int, string) {
			switch kind {
			case "profile":
				return 200, data(`{"user":{"createdAt":"2014-05-01T00:00:00Z",` +
					`"contributionsCollection":{"contributionYears":[2026,2023,2025,2024]}}}`)
			case "repos":
				if vars["cursor"] == nil {
					return 200, data(`{"user":{"repositories":` + repoPageJSON(true, "c1",
						repoNodeJSON("example-user", "alpha", "example-user/alpha", false, "Go:100", "PHP:50"),
						repoNodeJSON("example-user", "beta", "example-user/beta", false, "Rust:10"),
					) + `}}`)
				}
				return 200, data(`{"user":{"repositories":` + repoPageJSON(false, "",
					repoNodeJSON("example-user", "beta", "example-user/beta", false, "Rust:10"),
					repoNodeJSON("example-user", "charlie", "", true, "C:5"),
				) + `}}`)
			case "org":
				if vars["cursor"] == nil {
					return 200, data(`{"organization":{"repositories":` + repoPageJSON(true, "o1",
						repoNodeJSON("some-org", "omega", "some-org/omega", false, "Kotlin:7"),
					) + `}}`)
				}
				return 200, data(`{"organization":{"repositories":` + repoPageJSON(true, "",
					repoNodeJSON("example-user", "alpha", "example-user/alpha", false, "Go:100"),
				) + `}}`)
			case "calendar":
				year := vars["from"].(string)[:4]
				return 200, data(`{"user":{"contributionsCollection":{"contributionCalendar":{"weeks":[` +
					`{"contributionDays":[{"date":"` + year + `-01-01","contributionCount":1},` +
					`{"date":"` + year + `-01-02","contributionCount":0}]},` +
					`{"contributionDays":[{"date":"` + year + `-01-03","contributionCount":4}]}]}}}}`)
			}
			return 500, "unexpected"
		},
		rest: map[string]restReply{
			"/repos/example-user/alpha/git/trees/HEAD?recursive=1": {200, treeJSON(true,
				blobEntry("dist/app.js", 100, "s0"),
				`{"path":"themes","type":"tree","size":0,"sha":"t1"}`,
				blobEntry("themes/a.php", 30, "s1"),
				blobEntry("themes/b.PHP", 20, "s2"),
				blobEntry("themes/c.php", 10, "s3"),
				blobEntry("distx/y.js", 5, "s4"),
				blobEntry("README.md", 5, "s5"),
			)},
			"/repos/example-user/alpha/git/blobs/s1": {200, `{"content":"` + strings.ReplaceAll(b64, "\n", `\n`) + `","encoding":"base64"}`},
			"/repos/example-user/alpha/git/blobs/s2": {200, `{"content":"no tag here","encoding":"utf-8"}`},
			"/repos/some-org/omega/git/trees/HEAD?recursive=1": {200, treeJSON(false,
				blobEntry("src/main.kt", 7, "k1"),
			)},
			"/repos/example-user/delta/git/trees/HEAD?recursive=1": {409, `{"message":"Git Repository is empty."}`},
		},
	}
}

func fullFetchOptions(loc *time.Location) Options {
	return Options{
		Username:           "example-user",
		Location:           loc,
		FetchLanguages:     true,
		FetchStreak:        true,
		StartYear:          2024,
		Now:                time.Date(2025, 6, 15, 12, 0, 0, 0, loc),
		Organizations:      []string{"some-org", "  "},
		Visibility:         "Public",
		GlobalExcludePaths: []string{"./dist", " "},
		ExcludePaths: map[string][]string{
			"alpha":     {"themes/"},
			"delta":     {"lib/"},
			"ghostrepo": {"x/"},
			"omega":     {" "},
		},
		ExcludeRepos:     []string{"beta"},
		ExcludeArchived:  true,
		ResolveHackFiles: true,
		HackFileLimit:    2,
	}
}

func TestFetchEndToEnd(t *testing.T) {
	f := fullFetchServer()
	c, _ := newTestClient(t, f, 0)
	loc := time.UTC

	got, err := c.Fetch(context.Background(), fullFetchOptions(loc))
	if err != nil {
		t.Fatal(err)
	}

	if want := wantFullFetch(); !reflect.DeepEqual(got, want) {
		gotJSON, _ := json.MarshalIndent(got, "", " ")
		wantJSON, _ := json.MarshalIndent(want, "", " ")
		t.Fatalf("data mismatch\n got: %s\nwant: %s", gotJSON, wantJSON)
	}
	checkRepoCalls(t, f)
	checkOrgCalls(t, f)
	checkCalendarCalls(t, f)
	for _, p := range f.paths {
		if strings.Contains(p, "/beta/") || strings.Contains(p, "/charlie/") || strings.Contains(p, "/blobs/s3") {
			t.Errorf("read something that should be skipped: %s", p)
		}
	}
}

// wantFullFetch is what fullFetchServer and fullFetchOptions should produce.
func wantFullFetch() *Data {
	lang := func(name string, size int64) Lang { return Lang{Name: name, Color: "#123456", Size: size} }
	cal := func(year int) Calendar {
		y := strconv.Itoa(year)
		return Calendar{Year: year, Days: []Day{{y + "-01-01", 1}, {y + "-01-02", 0}, {y + "-01-03", 4}}}
	}
	return &Data{
		FetchedAt:         time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC),
		Username:          "example-user",
		UserCreatedAt:     time.Date(2014, 5, 1, 0, 0, 0, 0, time.UTC),
		Timezone:          "UTC",
		ContributionYears: []int{2024, 2025, 2026},
		Repositories: []Repo{
			{Name: "alpha", Owner: "example-user", NameWithOwner: "example-user/alpha",
				Languages: []Lang{lang("Go", 100), lang("PHP", 50)}},
			{Name: "beta", Owner: "example-user", NameWithOwner: "example-user/beta",
				Languages: []Lang{lang("Rust", 10)}},
			{Name: "charlie", Owner: "example-user", Archived: true,
				Languages: []Lang{lang("C", 5)}},
			{Name: "omega", Owner: "some-org", NameWithOwner: "some-org/omega",
				Languages: []Lang{lang("Kotlin", 7)}},
		},
		Calendars: []Calendar{cal(2024), cal(2025)},
		Trees: []RepoTree{
			{Repo: "alpha", Truncated: true, Prefixes: []string{"dist", "themes/"}, Files: []TreeFile{
				{Path: "dist/app.js", Size: 100},
				{Path: "themes/a.php", Size: 30, Language: "PHP"},
				{Path: "themes/b.PHP", Size: 20, Language: "Hack"},
				{Path: "themes/c.php", Size: 10},
			}},
			// Kept although nothing matched: its raw per-repo entry is
			// non-empty, even though that entry normalises to nothing.
			{Repo: "omega", Prefixes: []string{"dist"}},
			{Repo: "delta", Prefixes: []string{"lib/"}},
			{Repo: "ghostrepo", Prefixes: []string{"x/"}},
		},
	}
}

func checkRepoCalls(t *testing.T, f *fakeGitHub) {
	t.Helper()
	repoCalls := f.callsOf("repos")
	if len(repoCalls) != 2 {
		t.Fatalf("repo pages: got %d, want 2", len(repoCalls))
	}
	first, second := repoCalls[0].vars, repoCalls[1].vars
	if first["privacy"] != "PUBLIC" || first["langs"] != float64(50) || first["login"] != "example-user" {
		t.Errorf("first repo page vars: %v", first)
	}
	if !reflect.DeepEqual(first["affiliations"], []any{"OWNER"}) {
		t.Errorf("default affiliations: %v", first["affiliations"])
	}
	if _, ok := first["cursor"]; ok {
		t.Errorf("first page sent a cursor: %v", first)
	}
	if second["cursor"] != "c1" {
		t.Errorf("second page cursor: %v", second["cursor"])
	}
}

func checkOrgCalls(t *testing.T, f *fakeGitHub) {
	t.Helper()
	orgCalls := f.callsOf("org")
	if len(orgCalls) != 2 {
		t.Fatalf("org pages: got %d, want 2 (blank org skipped, empty cursor stops)", len(orgCalls))
	}
	if orgCalls[0].vars["org"] != "some-org" || orgCalls[0].vars["privacy"] != "PUBLIC" || orgCalls[1].vars["cursor"] != "o1" {
		t.Errorf("org vars: %v / %v", orgCalls[0].vars, orgCalls[1].vars)
	}
}

func checkCalendarCalls(t *testing.T, f *fakeGitHub) {
	t.Helper()
	calCalls := f.callsOf("calendar")
	if len(calCalls) != 2 {
		t.Fatalf("calendar calls: got %d, want 2 (future year skipped)", len(calCalls))
	}
	if calCalls[0].vars["from"] != "2024-01-01T00:00:00Z" || calCalls[0].vars["to"] != "2024-12-31T23:59:59Z" {
		t.Errorf("2024 window: %v", calCalls[0].vars)
	}
	if calCalls[1].vars["to"] != "2025-06-15T12:00:00Z" {
		t.Errorf("2025 window not clamped to now: %v", calCalls[1].vars)
	}
}

func TestFetchRequiresLocation(t *testing.T) {
	c := New("http://127.0.0.1:1/graphql", "t", "ua", time.Second, 0)
	_, err := c.Fetch(context.Background(), Options{})
	if err == nil || err.Error() != "fetch: location is required" {
		t.Fatalf("got %v", err)
	}
}

func TestFetchProfileOnlyDefaultsNow(t *testing.T) {
	f := fullFetchServer()
	c, _ := newTestClient(t, f, 0)
	before := time.Now().UTC()
	got, err := c.Fetch(context.Background(), Options{Username: "example-user", Location: time.UTC})
	if err != nil {
		t.Fatal(err)
	}
	if got.FetchedAt.Before(before.Add(-time.Second)) || got.Repositories != nil || got.Calendars != nil || got.Trees != nil {
		t.Fatalf("unexpected: %+v", got)
	}
	if !reflect.DeepEqual(got.ContributionYears, []int{2023, 2024, 2025, 2026}) {
		t.Errorf("years: %v", got.ContributionYears)
	}
	if len(f.calls) != 1 {
		t.Errorf("calls: %d", len(f.calls))
	}
}

func TestFetchNoTreesWithoutExclusions(t *testing.T) {
	f := fullFetchServer()
	c, _ := newTestClient(t, f, 0)
	got, err := c.Fetch(context.Background(), Options{Username: "example-user", Location: time.UTC, FetchLanguages: true,
		OwnerAffiliations: []string{"OWNER", "COLLABORATOR"}, LanguagesPerRepo: 7})
	if err != nil {
		t.Fatal(err)
	}
	if got.Trees != nil || len(f.paths) != 0 {
		t.Fatalf("trees read without exclusions: %v", f.paths)
	}
	v := f.callsOf("repos")[0].vars
	if _, ok := v["privacy"]; ok || v["langs"] != float64(7) || !reflect.DeepEqual(v["affiliations"], []any{"OWNER", "COLLABORATOR"}) {
		t.Errorf("vars: %v", v)
	}
}

func TestFetchErrorsAreWrapped(t *testing.T) {
	cases := []struct {
		name string
		fail string
		opt  func(*Options)
		want string
	}{
		{"profile", "profile", nil, "profile: HTTP 400: nope"},
		{"repos", "repos", nil, "repositories: HTTP 400: nope"},
		{"org", "org", nil, "repositories: organization some-org: HTTP 400: nope"},
		{"calendar", "calendar", nil, "calendars: year 2024: HTTP 400: nope"},
		{"trees", "tree", nil, "path exclusions: repo alpha: HTTP 400: nope"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := fullFetchServer()
			inner := f.graphql
			f.graphql = func(kind string, vars map[string]any) (int, string) {
				if kind == tc.fail {
					return 400, "nope"
				}
				return inner(kind, vars)
			}
			if tc.fail == "tree" {
				f.rest["/repos/example-user/alpha/git/trees/HEAD?recursive=1"] = restReply{400, "nope"}
			}
			c, _ := newTestClient(t, f, 0)
			_, err := c.Fetch(context.Background(), fullFetchOptions(time.UTC))
			if err == nil || err.Error() != tc.want {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
		})
	}
}

func TestFetchTreesPerRepoOnly(t *testing.T) {
	f := &fakeGitHub{rest: map[string]restReply{
		"/repos/some-org/omega/git/trees/HEAD?recursive=1": {200, treeJSON(false, blobEntry("lib/a.go", 3, "x"))},
		"/repos/example-user/beta/git/trees/HEAD?recursive=1": {200, treeJSON(false,
			blobEntry("lib/a.php", 3, "p1"))},
		"/repos/example-user/beta/git/blobs/p1": {500, "boom"},
	}}
	c, _ := newTestClient(t, f, 0)
	got, err := c.fetchTrees(context.Background(), Options{
		Username:         "example-user",
		ExcludePaths:     map[string][]string{"some-org/omega": {"lib"}, "beta": {"lib/"}},
		ResolveHackFiles: true,
		HackFileLimit:    -1,
		repos: []Repo{
			{Name: "omega", Owner: "some-org", NameWithOwner: "some-org/omega"},
			{Name: "beta"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	// The global sweep is off, so the qualified key resolves to the repo and
	// the tree is reported under the bare name. Its files are kept only when
	// the bare name has its own entry, which here it does not.
	want := []RepoTree{
		{Repo: "beta", Prefixes: []string{"lib/"}, Files: []TreeFile{{Path: "lib/a.php", Size: 3}}},
		{Repo: "omega", Prefixes: []string{"lib"}, Files: []TreeFile{{Path: "lib/a.go", Size: 3}}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}

func TestFetchTreesDropsUnmatchedGlobalTree(t *testing.T) {
	f := &fakeGitHub{rest: map[string]restReply{
		"/repos/example-user/beta/git/trees/HEAD?recursive=1": {200, treeJSON(false, blobEntry("src/a.go", 3, "x"))},
	}}
	c, _ := newTestClient(t, f, 0)
	got, err := c.fetchTrees(context.Background(), Options{
		Username:           "example-user",
		GlobalExcludePaths: []string{"dist/"},
		repos:              []Repo{{Name: "beta"}},
	})
	if err != nil || len(got) != 0 {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestBlobReadFailureLeavesLanguageEmpty(t *testing.T) {
	f := &fakeGitHub{rest: map[string]restReply{
		"/repos/example-user/beta/git/trees/HEAD?recursive=1": {200, treeJSON(false,
			blobEntry("lib/a.php", 3, "p1"), blobEntry("lib/b.php", 3, "p2"))},
		"/repos/example-user/beta/git/blobs/p1": {400, "bad"},
		"/repos/example-user/beta/git/blobs/p2": {200, `{"content":"!!!","encoding":"base64"}`},
	}}
	c, _ := newTestClient(t, f, 0)
	got, err := c.fetchTrees(context.Background(), Options{
		Username:         "example-user",
		ExcludePaths:     map[string][]string{"beta": {"lib/"}},
		ResolveHackFiles: true,
		HackFileLimit:    5,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []RepoTree{{Repo: "beta", Prefixes: []string{"lib/"}, Files: []TreeFile{{Path: "lib/a.php", Size: 3}, {Path: "lib/b.php", Size: 3}}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}
}

func TestBlobHasPHPTag(t *testing.T) {
	f := &fakeGitHub{rest: map[string]restReply{
		"/repos/o/r/git/blobs/a": {200, `{"content":"PD9waHA=","encoding":"base64"}`},
		"/repos/o/r/git/blobs/b": {200, `{"content":"aGVsbG8=","encoding":"base64"}`},
		"/repos/o/r/git/blobs/c": {200, `{"content":"x <?php","encoding":"utf-8"}`},
		"/repos/o/r/git/blobs/d": {200, `{"content":"%%%","encoding":"base64"}`},
	}}
	c, _ := newTestClient(t, f, 0)
	for sha, want := range map[string]bool{"a": true, "b": false, "c": true} {
		got, err := c.blobHasPHPTag(context.Background(), "o", "r", sha)
		if err != nil || got != want {
			t.Errorf("%s: got %v, %v", sha, got, err)
		}
	}
	if _, err := c.blobHasPHPTag(context.Background(), "o", "r", "d"); err == nil || !strings.HasPrefix(err.Error(), "decode blob: ") {
		t.Errorf("bad base64: %v", err)
	}
}

// statusServer answers every request with one status, headers and body.
func statusServer(status int, header http.Header, body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		maps.Copy(w.Header(), header)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	})
}

func TestDoStatusHandling(t *testing.T) {
	limited := http.Header{"Retry-After": {"7"}}
	cases := []struct {
		name      string
		status    int
		header    http.Header
		body      string
		want      string
		retryable bool
		after     time.Duration
	}{
		{"unauthorized", 401, nil, "bad credentials\nmore", "github rejected the token (HTTP 401): bad credentials", false, 0},
		{"forbidden", 403, nil, "nope", "github rejected the token (HTTP 403): nope", false, 0},
		{"forbidden rate limited", 403, limited, "", "rate limited (HTTP 403)", true, 7 * time.Second},
		{"unauthorized rate limited", 401, limited, "", "rate limited (HTTP 401)", true, 7 * time.Second},
		{"too many", 429, limited, "", "HTTP 429", true, 7 * time.Second},
		{"too many no header", 429, nil, "", "HTTP 429", true, 0},
		{"server", 502, nil, "bad gateway", "HTTP 502: bad gateway", true, 0},
		{"teapot", 418, nil, "short and stout", "HTTP 418: short and stout", false, 0},
		{"bad json", 200, nil, "{", "decode response: unexpected end of JSON input", true, 0},
		{"graphql error", 200, nil, `{"errors":[{"type":"NOT_FOUND","message":"a"},{"message":"b"}]}`, "graphql: a; b", false, 0},
		{"rate limited type", 200, nil, `{"errors":[{"type":"RATE_LIMITED","message":"slow"}]}`, "graphql: slow", true, time.Minute},
		{"secondary", 200, nil, `{"errors":[{"message":"You have exceeded a secondary rate limit"}]}`, "graphql: You have exceeded a secondary rate limit", true, time.Minute},
		{"missing data", 200, nil, `{}`, "graphql returned no data", true, 0},
		{"decode data", 200, nil, `{"data":"str"}`, "decode data: json: cannot unmarshal string into Go value of type struct { X int }", false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newTestClient(t, statusServer(tc.status, tc.header, tc.body), 0)
			var out struct{ X int }
			err := c.do(context.Background(), []byte(`{}`), &out)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
			re, ok := errors.AsType[*retryableError](err)
			if ok != tc.retryable {
				t.Fatalf("retryable: got %v, want %v", ok, tc.retryable)
			}
			if ok && re.after != tc.after {
				t.Errorf("after: got %v, want %v", re.after, tc.after)
			}
		})
	}
}

func TestDoSendsHeadersAndDecodes(t *testing.T) {
	var got http.Header
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		_, _ = io.WriteString(w, `{"data":{"X":3}}`)
	})
	c, _ := newTestClient(t, h, 0)
	var out struct{ X int }
	if err := c.do(context.Background(), []byte(`{}`), &out); err != nil || out.X != 3 {
		t.Fatalf("got %v, %v", out, err)
	}
	if got.Get("Authorization") != "Bearer test-token" || got.Get("Content-Type") != "application/json" ||
		got.Get("Accept") != "application/json" || got.Get("User-Agent") != "gitdisplay-test" {
		t.Errorf("headers: %v", got)
	}
}

func TestDoBadEndpointAndTransportError(t *testing.T) {
	c := New("://bad", "t", "ua", time.Second, 0)
	if err := c.do(context.Background(), nil, nil); err == nil || !strings.HasPrefix(err.Error(), "build request: ") {
		t.Errorf("bad endpoint: %v", err)
	}
	c = New("http://127.0.0.1:1/graphql", "t", "ua", time.Second, 0)
	err := c.do(context.Background(), nil, nil)
	if _, ok := errors.AsType[*retryableError](err); !ok || !strings.HasPrefix(err.Error(), "request: ") {
		t.Errorf("transport: %v", err)
	}
}

// sequenceServer answers with each status in turn, then repeats the last.
func sequenceServer(statuses []int, header http.Header, body string) (http.Handler, *int) {
	n := 0
	var mu sync.Mutex
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		i := min(n, len(statuses)-1)
		n++
		mu.Unlock()
		maps.Copy(w.Header(), header)
		w.WriteHeader(statuses[i])
		if statuses[i] == 200 {
			_, _ = io.WriteString(w, body)
		}
	}), &n
}

func TestQueryRetries(t *testing.T) {
	h, n := sequenceServer([]int{500, 429, 200}, http.Header{"Retry-After": {"3"}}, `{"data":{"X":1}}`)
	c, delays := newTestClient(t, h, 3)
	var out struct{ X int }
	if err := c.query(context.Background(), "q", nil, &out); err != nil || out.X != 1 {
		t.Fatalf("got %v, %v", out, err)
	}
	if *n != 3 {
		t.Errorf("attempts: %d", *n)
	}
	if !reflect.DeepEqual(*delays, []time.Duration{time.Second, 3 * time.Second}) {
		t.Errorf("delays: %v", *delays)
	}
}

func TestQueryRetriesExhausted(t *testing.T) {
	h, n := sequenceServer([]int{503}, nil, "")
	c, delays := newTestClient(t, h, 2)
	err := c.query(context.Background(), "q", nil, &struct{}{})
	if err == nil || err.Error() != "after 3 attempts: HTTP 503: " {
		t.Fatalf("got %v", err)
	}
	if *n != 3 || !reflect.DeepEqual(*delays, []time.Duration{time.Second, 2 * time.Second}) {
		t.Errorf("attempts %d, delays %v", *n, *delays)
	}
}

func TestQueryStopsOnPermanentError(t *testing.T) {
	h, n := sequenceServer([]int{401}, nil, "")
	c, _ := newTestClient(t, h, 3)
	if err := c.query(context.Background(), "q", nil, &struct{}{}); err == nil || *n != 1 {
		t.Fatalf("got %v after %d attempts", err, *n)
	}
}

func TestQueryEncodeError(t *testing.T) {
	c, _ := newTestClient(t, statusServer(200, nil, ""), 0)
	err := c.query(context.Background(), "q", map[string]any{"x": make(chan int)}, nil)
	if err == nil || !strings.HasPrefix(err.Error(), "encode request: ") {
		t.Fatalf("got %v", err)
	}
}

func TestQueryContextCancelled(t *testing.T) {
	c, _ := newTestClient(t, statusServer(200, nil, `{"data":{}}`), 3)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.query(ctx, "q", nil, &struct{}{}); err != context.Canceled {
		t.Fatalf("got %v", err)
	}
}

func TestQueryContextCancelledDuringWait(t *testing.T) {
	h, _ := sequenceServer([]int{500}, nil, "")
	c, _ := newTestClient(t, h, 3)
	ctx, cancel := context.WithCancel(context.Background())
	c.after = func(time.Duration) <-chan time.Time {
		cancel()
		return nil
	}
	if err := c.query(ctx, "q", nil, &struct{}{}); err != context.Canceled {
		t.Fatalf("got %v", err)
	}
}

func TestRealWaitIsUsedByDefault(t *testing.T) {
	c := New("http://x", "t", "ua", time.Second, 0)
	select {
	case <-c.wait(time.Millisecond):
	case <-time.After(5 * time.Second):
		t.Fatal("default wait never fired")
	}
}

func TestRESTStatusHandling(t *testing.T) {
	cases := []struct {
		status    int
		header    http.Header
		body      string
		want      string
		notFound  bool
		retryable bool
	}{
		{404, nil, "missing", "not found: missing", true, false},
		{409, nil, "empty", "nothing to read: empty", true, false},
		{401, nil, "no", "github rejected the token (HTTP 401): no", false, false},
		{403, http.Header{"Retry-After": {"1"}}, "", "rate limited (HTTP 403)", false, true},
		{429, nil, "", "HTTP 429", false, true},
		{500, nil, "x", "HTTP 500: x", false, true},
		{418, nil, "t", "HTTP 418: t", false, false},
		{200, nil, "{", "decode response: unexpected end of JSON input", false, false},
	}
	for _, tc := range cases {
		t.Run(strconv.Itoa(tc.status)+tc.want, func(t *testing.T) {
			c, _ := newTestClient(t, statusServer(tc.status, tc.header, tc.body), 0)
			err := c.doREST(context.Background(), c.Endpoint, &struct{}{})
			if err == nil || err.Error() != tc.want {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
			if _, ok := errors.AsType[*notFoundError](err); ok != tc.notFound {
				t.Errorf("not found: %v", ok)
			}
			if _, ok := errors.AsType[*retryableError](err); ok != tc.retryable {
				t.Errorf("retryable: %v", ok)
			}
		})
	}
}

func TestRESTHeadersAndTarget(t *testing.T) {
	var got *http.Request
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(context.Background())
		_, _ = io.WriteString(w, `{"ok":true}`)
	})
	c, _ := newTestClient(t, h, 0)
	var out struct{ OK bool }
	if err := c.rest(context.Background(), "/repos/o/r/x?y=1", &out); err != nil || !out.OK {
		t.Fatalf("got %v, %v", out, err)
	}
	if got.URL.RequestURI() != "/repos/o/r/x?y=1" || got.Header.Get("Accept") != "application/vnd.github+json" ||
		got.Header.Get("X-GitHub-Api-Version") != "2022-11-28" || got.Header.Get("Authorization") != "Bearer test-token" ||
		got.Header.Get("User-Agent") != "gitdisplay-test" {
		t.Errorf("request: %s %v", got.URL, got.Header)
	}
}

func TestRESTRetries(t *testing.T) {
	h, n := sequenceServer([]int{500, 200}, nil, `{}`)
	c, delays := newTestClient(t, h, 2)
	if err := c.rest(context.Background(), "/x", &struct{}{}); err != nil || *n != 2 {
		t.Fatalf("got %v after %d", err, *n)
	}
	if !reflect.DeepEqual(*delays, []time.Duration{time.Second}) {
		t.Errorf("delays: %v", *delays)
	}

	h, _ = sequenceServer([]int{500}, nil, "")
	c, _ = newTestClient(t, h, 1)
	if err := c.rest(context.Background(), "/x", &struct{}{}); err == nil || err.Error() != "after 2 attempts: HTTP 500: " {
		t.Errorf("exhausted: %v", err)
	}

	h, n = sequenceServer([]int{404}, nil, "")
	c, _ = newTestClient(t, h, 3)
	if err := c.rest(context.Background(), "/x", &struct{}{}); err == nil || *n != 1 {
		t.Errorf("permanent: %v after %d", err, *n)
	}
}

func TestRESTHonoursRetryAfterAndContext(t *testing.T) {
	h, _ := sequenceServer([]int{429, 200}, http.Header{"Retry-After": {"9"}}, `{}`)
	c, delays := newTestClient(t, h, 1)
	if err := c.rest(context.Background(), "/x", &struct{}{}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(*delays, []time.Duration{9 * time.Second}) {
		t.Errorf("delays: %v", *delays)
	}

	h, _ = sequenceServer([]int{500}, nil, "")
	c, _ = newTestClient(t, h, 3)
	ctx, cancel := context.WithCancel(context.Background())
	c.after = func(time.Duration) <-chan time.Time {
		cancel()
		return nil
	}
	if err := c.rest(ctx, "/x", &struct{}{}); err != context.Canceled {
		t.Errorf("cancel: %v", err)
	}
}

func TestRESTBadEndpoint(t *testing.T) {
	c := New("http://[::1", "t", "ua", time.Second, 0)
	if err := c.rest(context.Background(), "/x", nil); err == nil || !strings.HasPrefix(err.Error(), `bad endpoint "http://[::1": `) {
		t.Errorf("got %v", err)
	}
	c = New("http://127.0.0.1:1/graphql", "t", "ua", time.Second, 0)
	if err := c.doREST(context.Background(), "://bad", nil); err == nil || !strings.HasPrefix(err.Error(), "build request: ") {
		t.Errorf("build: %v", err)
	}
	err := c.doREST(context.Background(), "http://127.0.0.1:1/x", nil)
	if _, ok := errors.AsType[*retryableError](err); !ok {
		t.Errorf("transport: %v", err)
	}
}

func TestRateLimitWait(t *testing.T) {
	resp := func(h map[string]string) *http.Response {
		r := &http.Response{Header: http.Header{}}
		for k, v := range h {
			r.Header.Set(k, v)
		}
		return r
	}
	unix := func(d time.Duration) string { return strconv.FormatInt(time.Now().Add(d).Unix(), 10) }

	cases := []struct {
		name     string
		headers  map[string]string
		ok       bool
		min, max time.Duration
	}{
		{"retry-after", map[string]string{"Retry-After": "12"}, true, 12 * time.Second, 12 * time.Second},
		{"retry-after zero", map[string]string{"Retry-After": "0"}, true, 0, 0},
		{"negative retry-after", map[string]string{"Retry-After": "-1"}, false, 0, 0},
		{"non-numeric retry-after", map[string]string{"Retry-After": "soon"}, false, 0, 0},
		{"reset in the past", map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": unix(-time.Hour)}, true, time.Second, time.Second},
		{"reset soon", map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": unix(2 * time.Minute)}, true, 100 * time.Second, 122 * time.Second},
		{"an hour wait", map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": unix(time.Hour)}, false, 0, 0},
		{"bad reset", map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": "never"}, false, 0, 0},
		{"missing reset", map[string]string{"X-RateLimit-Remaining": "0"}, false, 0, 0},
		{"quota remaining", map[string]string{"X-RateLimit-Remaining": "5", "X-RateLimit-Reset": unix(time.Minute)}, false, 0, 0},
	}
	for _, tc := range cases {
		d, ok := rateLimitWait(resp(tc.headers))
		if ok != tc.ok {
			t.Errorf("%s: ok = %v, want %v", tc.name, ok, tc.ok)
			continue
		}
		if ok && (d < tc.min || d > tc.max) {
			t.Errorf("%s: wait %v, want between %v and %v", tc.name, d, tc.min, tc.max)
		}
	}
}

func TestBackoff(t *testing.T) {
	want := map[int]time.Duration{-3: time.Second, 0: time.Second, 1: time.Second, 2: 2 * time.Second,
		3: 4 * time.Second, 5: 16 * time.Second, 6: 30 * time.Second, 99: 30 * time.Second}
	for attempt, d := range want {
		if got := backoff(attempt); got != d {
			t.Errorf("backoff(%d) = %v, want %v", attempt, got, d)
		}
	}
}

func TestFirstLine(t *testing.T) {
	long := strings.Repeat("a", 301)
	cases := map[string]string{"  one\ntwo ": "one", "": "", long: strings.Repeat("a", 300) + "..."}
	for in, want := range cases {
		if got := firstLine([]byte(in)); got != want {
			t.Errorf("firstLine(%q) = %q", in, got)
		}
	}
}

func TestPathHelpers(t *testing.T) {
	if got := normalizePrefixes([]string{" ./b/", "/a", "", "  ", "c"}); !reflect.DeepEqual(got, []string{"a", "b/", "c"}) {
		t.Errorf("normalizePrefixes: %v", got)
	}
	matches := []struct {
		path string
		want bool
	}{{"themes/vdg/a.css", true}, {"themes/vdg", true}, {"themes/vdgx/a.css", false}, {"other", false}}
	for _, m := range matches {
		if got := matchesPrefix(m.path, []string{"x/", "themes/vdg"}); got != m.want {
			t.Errorf("matchesPrefix(%q) = %v", m.path, got)
		}
	}
	exts := map[string]string{"a/b.php": ".php", "a.b/c": "", "noext": "", "x.tar.gz": ".gz"}
	for in, want := range exts {
		if got := pathExt(in); got != want {
			t.Errorf("pathExt(%q) = %q", in, got)
		}
	}
	if got := mergePrefixes([]string{"b", "a"}, []string{"a", "c"}); !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
		t.Errorf("mergePrefixes: %v", got)
	}
	if got := sortedKeysOf(map[string][]string{"b": nil, "a": nil}); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("sortedKeysOf: %v", got)
	}
}

func TestLoadDump(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.json")
	if err := os.WriteFile(good, []byte(`{"username":"example-user","contribution_years":[2025]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	d, err := LoadDump(good)
	if err != nil || d.Username != "example-user" || !reflect.DeepEqual(d.ContributionYears, []int{2025}) {
		t.Fatalf("got %+v, %v", d, err)
	}

	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte(`{`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadDump(bad); err == nil || !strings.HasPrefix(err.Error(), "parse dump "+bad+": ") {
		t.Errorf("bad: %v", err)
	}
	if _, err := LoadDump(filepath.Join(dir, "missing.json")); err == nil {
		t.Error("missing file loaded")
	}
}

func TestNewConfiguresClient(t *testing.T) {
	c := New("https://example.test/graphql", "tok", "ua", 7*time.Second, 3)
	if c.Endpoint != "https://example.test/graphql" || c.Token != "tok" || c.UserAgent != "ua" || c.MaxRetries != 3 || c.HTTP.Timeout != 7*time.Second {
		t.Errorf("client: %+v", c)
	}
	tr := c.HTTP.Transport.(*http.Transport)
	if tr.MaxIdleConns != 4 || tr.MaxConnsPerHost != 4 || tr.IdleConnTimeout != 90*time.Second {
		t.Errorf("transport: %+v", tr)
	}
}
