// Package stats aggregates the fetched GitHub data into the results the
// cards render: language rankings and contribution streaks.
package stats

import (
	"fmt"
	"sort"
	"strings"

	"github.com/thesimpledev/gitdisplay/internal/config"
	"github.com/thesimpledev/gitdisplay/internal/gh"
)

// LangStat is one language as it appears on the card.
type LangStat struct {
	Name    string  `json:"name"`
	Color   string  `json:"color"`
	Size    int64   `json:"size"`
	Percent float64 `json:"percent"`
}

// Subtraction records bytes removed by a path exclusion, so the result can be
// checked rather than taken on faith.
type Subtraction struct {
	Repo     string `json:"repo"`
	Language string `json:"language"`
	Bytes    int64  `json:"bytes"`
	Files    int    `json:"files"`
	// Clamped is set when the subtraction was larger than the bytes GitHub
	// reported for that language, so it was capped at zero.
	Clamped bool `json:"clamped"`
}

// LangResult is the finished languages computation.
type LangResult struct {
	Languages []LangStat `json:"languages"`
	// ReposConsidered is how many repositories contributed bytes.
	ReposConsidered int `json:"repos_considered"`
	// ReposFetched is how many came back from the API.
	ReposFetched int `json:"repos_fetched"`
	// DisplayedBytes is the denominator used for the percentages.
	DisplayedBytes int64 `json:"displayed_bytes"`
	// Warnings lists config entries that matched nothing, plus anything else
	// worth a human's attention.
	Warnings     []string      `json:"warnings,omitempty"`
	Subtractions []Subtraction `json:"subtractions,omitempty"`
	// ArchivedSkipped names the repositories dropped by exclude_archived.
	ArchivedSkipped []string `json:"archived_skipped,omitempty"`
}

// SplitResult is the two halves of the split card: what you work on now and
// what you used to work on.
type SplitResult struct {
	Active   LangResult `json:"active"`
	Archived LangResult `json:"archived"`
}

// Languages aggregates repository language bytes into the card's rows.
func Languages(data *gh.Data, cfg config.Languages) LangResult {
	return aggregate(data, cfg, func(r gh.Repo) bool {
		return !(cfg.ExcludeArchived && r.Archived)
	}, true)
}

// LanguagesSplit aggregates active and archived repositories separately.
//
// Each half gets its own top-N and its own percentages, so the two lists are
// read against themselves rather than against a combined total. That is what
// makes the comparison meaningful: the archived half is a smaller body of code
// and would otherwise barely register.
func LanguagesSplit(data *gh.Data, cfg config.Languages) SplitResult {
	active := aggregate(data, cfg, func(r gh.Repo) bool { return !r.Archived }, true)

	archivedCfg := cfg
	if cfg.ArchivedCount > 0 {
		archivedCfg.Count = cfg.ArchivedCount
	}
	// The warnings are the same both ways round; reporting them once is enough.
	archived := aggregate(data, archivedCfg, func(r gh.Repo) bool { return r.Archived }, false)

	// Nothing was skipped: the archived repositories are on the card, in the
	// lower section. Reporting them as dropped would be wrong.
	active.ArchivedSkipped = nil
	archived.ArchivedSkipped = nil
	return SplitResult{Active: active, Archived: archived}
}

func aggregate(data *gh.Data, cfg config.Languages, include func(gh.Repo) bool, withWarnings bool) LangResult {
	res := LangResult{ReposFetched: len(data.Repositories)}

	repos := data.Repositories
	// MaxRepos reproduces the original tool's unpaginated behaviour: it read
	// the first page and stopped. Left at 0 every repository is used.
	if cfg.MaxRepos > 0 && len(repos) > cfg.MaxRepos {
		repos = repos[:cfg.MaxRepos]
	}

	subtractions, subByRepo := pathSubtractions(data, cfg)
	res.Subtractions = subtractions

	t := newTally(cfg, subByRepo)
	for _, repo := range repos {
		t.noteLanguages(repo)
		if t.isExcluded(repo) {
			continue
		}
		if !include(repo) {
			if repo.Archived {
				res.ArchivedSkipped = append(res.ArchivedSkipped, repo.Name)
			}
			continue
		}
		if t.addRepo(repo) {
			res.ReposConsidered++
		}
	}

	res.Languages, res.DisplayedBytes = t.rank()
	if withWarnings {
		res.Warnings = validateLanguages(data, cfg, t.seen)
	}
	return res
}

// langAcc is one row being accumulated. Names keep the casing GitHub uses;
// lookups are case-insensitive.
type langAcc struct {
	name  string
	color string
	// largest is the biggest single contribution seen, so a merged row
	// takes the colour of whichever source dominates it.
	largest int64
	size    int64
}

// tally accumulates language bytes across repositories under one config.
type tally struct {
	cfg       config.Languages
	excluded  map[string]bool
	aliases   map[string]string
	hidden    map[string]bool
	subByRepo map[string]map[string]int64
	totals    map[string]*langAcc
	// seen records every language present in the fetched data before any
	// filtering, for the hide_languages check.
	seen map[string]bool
}

func newTally(cfg config.Languages, subByRepo map[string]map[string]int64) *tally {
	t := &tally{
		cfg:       cfg,
		excluded:  make(map[string]bool, len(cfg.ExcludeRepos)),
		aliases:   make(map[string]string, len(cfg.LanguageAliases)),
		hidden:    make(map[string]bool, len(cfg.HideLanguages)),
		subByRepo: subByRepo,
		totals:    map[string]*langAcc{},
		seen:      map[string]bool{},
	}
	for _, name := range cfg.ExcludeRepos {
		t.excluded[name] = true
	}
	// Aliases merge one language into another before ranking, so the SQL
	// dialects GitHub reports separately can share a row.
	for from, to := range cfg.LanguageAliases {
		t.aliases[strings.ToLower(strings.TrimSpace(from))] = strings.TrimSpace(to)
	}
	for _, name := range cfg.HideLanguages {
		t.hidden[strings.ToLower(strings.TrimSpace(name))] = true
	}
	return t
}

// noteLanguages runs before any filtering. A hide entry for a language that
// only appears in archived repositories is still doing its job, so it must
// not be reported as matching nothing.
func (t *tally) noteLanguages(repo gh.Repo) {
	for _, lang := range repo.Languages {
		t.seen[strings.ToLower(lang.Name)] = true
	}
}

// isExcluded matches either the bare name or the qualified owner/name.
func (t *tally) isExcluded(r gh.Repo) bool {
	return t.excluded[r.Name] || (r.NameWithOwner != "" && t.excluded[r.NameWithOwner])
}

// addRepo adds one repository's languages and reports whether it had any.
func (t *tally) addRepo(repo gh.Repo) bool {
	langs := repo.Languages
	// LanguagesPerRepo reproduces the original's languages(first: 10),
	// which lost the tail. The API returns them largest first.
	if t.cfg.LanguagesPerRepo > 0 && len(langs) > t.cfg.LanguagesPerRepo {
		langs = langs[:t.cfg.LanguagesPerRepo]
	}
	if len(langs) == 0 {
		return false
	}
	overrides := lowerKeys(t.cfg.ByteOverrides[repo.Name])
	subs := t.subByRepo[repo.Name]
	for _, lang := range langs {
		t.addLang(lang, subs, overrides)
	}
	return true
}

func (t *tally) addLang(lang gh.Lang, subs, overrides map[string]int64) {
	key := strings.ToLower(lang.Name)
	t.seen[key] = true

	// Resolve the alias, then hide on either the reported name or the name
	// it merged into.
	display := lang.Name
	if to, ok := t.aliases[key]; ok && to != "" {
		display = to
	}
	mergedKey := strings.ToLower(display)
	if t.hidden[key] || t.hidden[mergedKey] {
		return
	}

	size := adjustedSize(lang.Size, key, subs, overrides)
	if size <= 0 {
		return
	}

	a := t.totals[mergedKey]
	if a == nil {
		a = &langAcc{name: display}
		t.totals[mergedKey] = a
	}
	if size > a.largest {
		a.largest, a.color = size, lang.Color
	}
	a.size += size
}

// adjustedSize applies a path subtraction and then any manual override. The
// override is the exact escape hatch and wins over a computed subtraction.
func adjustedSize(size int64, key string, subs, overrides map[string]int64) int64 {
	if sub, ok := subs[key]; ok {
		size = max(size-sub, 0)
	}
	if v, ok := overrides[key]; ok {
		size = v
	}
	return size
}

// rank orders the accumulated rows, keeps the top Count, and works out the
// percentages. It returns the rows and the denominator used.
func (t *tally) rank() ([]LangStat, int64) {
	// Hiding happens before the top-N cut, so a hidden language never uses up
	// one of the displayed slots.
	visible := make([]LangStat, 0, len(t.totals))
	var visibleBytes int64
	for key, a := range t.totals {
		if t.hidden[key] {
			continue
		}
		visible = append(visible, LangStat{Name: a.name, Color: a.color, Size: a.size})
		visibleBytes += a.size
	}

	sort.Slice(visible, func(i, j int) bool {
		if visible[i].Size != visible[j].Size {
			return visible[i].Size > visible[j].Size
		}
		return visible[i].Name < visible[j].Name
	})

	if t.cfg.Count > 0 && len(visible) > t.cfg.Count {
		visible = visible[:t.cfg.Count]
	}

	// The denominator is the sum of the displayed languages, so the card adds
	// up to 100%. Set percent_of_displayed to false to divide by every
	// non-hidden language instead.
	denom := visibleBytes
	if t.cfg.PercentOfDisplayed {
		denom = 0
		for _, l := range visible {
			denom += l.Size
		}
	}
	if denom > 0 {
		for i := range visible {
			visible[i].Percent = float64(visible[i].Size) / float64(denom) * 100
		}
	}
	return visible, denom
}

// repoIndex answers the lookups the config checks need.
type repoIndex struct {
	// names holds every bare and qualified name. Repositories beyond
	// MaxRepos still exist; naming one is not a typo.
	names    map[string]bool
	archived map[string]bool
	langs    map[string]map[string]bool
	trees    map[string]*gh.RepoTree
}

func newRepoIndex(data *gh.Data) repoIndex {
	idx := repoIndex{
		names:    make(map[string]bool, len(data.Repositories)*2),
		archived: map[string]bool{},
		langs:    map[string]map[string]bool{},
		trees:    map[string]*gh.RepoTree{},
	}
	for _, r := range data.Repositories {
		idx.names[r.Name] = true
		if r.NameWithOwner != "" {
			idx.names[r.NameWithOwner] = true
		}
		if r.Archived {
			idx.archived[r.Name] = true
		}
		set := make(map[string]bool, len(r.Languages))
		for _, l := range r.Languages {
			set[strings.ToLower(l.Name)] = true
		}
		idx.langs[r.Name] = set
	}
	for i := range data.Trees {
		idx.trees[data.Trees[i].Repo] = &data.Trees[i]
	}
	return idx
}

// validateLanguages reports every configured entry that matched nothing.
//
// A silent no-op here is how a typo disables an exclusion without anyone
// noticing, so each list is checked against the data actually fetched.
func validateLanguages(data *gh.Data, cfg config.Languages, seenLangs map[string]bool) []string {
	idx := newRepoIndex(data)
	var warns []string
	warns = append(warns, checkExcludeRepos(cfg, idx)...)
	warns = append(warns, checkHideLanguages(cfg.HideLanguages, seenLangs)...)
	warns = append(warns, checkByteOverrides(cfg.ByteOverrides, idx)...)
	warns = append(warns, checkExcludePaths(cfg.ExcludePaths, idx)...)
	return warns
}

func checkExcludeRepos(cfg config.Languages, idx repoIndex) []string {
	var warns []string
	for _, name := range cfg.ExcludeRepos {
		switch {
		case strings.TrimSpace(name) == "":
			warns = append(warns, "languages.exclude_repos: empty entry")
		case idx.names[name]:
			// Named a real repository. Fine even if MaxRepos cut it off.
			// Point out entries that exclude_archived already covers, so the
			// list can shrink to the repositories archiving does not handle.
			if cfg.ExcludeArchived && idx.archived[name] {
				warns = append(warns, fmt.Sprintf(
					"languages.exclude_repos: %q is archived and already excluded; the entry is redundant", name))
			}
		default:
			warns = append(warns, fmt.Sprintf(
				"languages.exclude_repos: %q matches no repository%s", name, nearestRepo(name, idx.names)))
		}
	}
	return warns
}

func checkHideLanguages(hide []string, seenLangs map[string]bool) []string {
	var warns []string
	for _, name := range hide {
		trimmed := strings.TrimSpace(name)
		if trimmed == "" {
			warns = append(warns, "languages.hide_languages: empty entry")
			continue
		}
		if !seenLangs[strings.ToLower(trimmed)] {
			warns = append(warns, fmt.Sprintf(
				"languages.hide_languages: %q matches no language in your repositories", name))
		}
	}
	return warns
}

func checkByteOverrides(overrides map[string]map[string]int64, idx repoIndex) []string {
	var warns []string
	for _, repo := range sortedKeys(overrides) {
		if !idx.names[repo] {
			warns = append(warns, fmt.Sprintf(
				"languages.byte_overrides: repository %q does not exist%s", repo, nearestRepo(repo, idx.names)))
			continue
		}
		for _, lang := range sortedKeys(overrides[repo]) {
			if !idx.langs[repo][strings.ToLower(strings.TrimSpace(lang))] {
				warns = append(warns, fmt.Sprintf(
					"languages.byte_overrides: %s has no language %q", repo, lang))
			}
		}
	}
	return warns
}

func checkExcludePaths(paths map[string][]string, idx repoIndex) []string {
	var warns []string
	for _, repo := range sortedKeys(paths) {
		if !idx.names[repo] {
			warns = append(warns, fmt.Sprintf(
				"languages.exclude_paths: repository %q does not exist%s", repo, nearestRepo(repo, idx.names)))
			continue
		}
		tree := idx.trees[repo]
		if tree == nil {
			warns = append(warns, fmt.Sprintf(
				"languages.exclude_paths: %s was not read; no paths were subtracted", repo))
			continue
		}
		if tree.Truncated {
			warns = append(warns, fmt.Sprintf(
				"languages.exclude_paths: %s tree was truncated by the API; subtraction is incomplete", repo))
		}
		warns = append(warns, checkPrefixes(repo, paths[repo], tree.Files)...)
	}
	return warns
}

func checkPrefixes(repo string, prefixes []string, files []gh.TreeFile) []string {
	var warns []string
	for _, prefix := range prefixes {
		trimmed := strings.TrimSpace(prefix)
		if trimmed == "" {
			warns = append(warns, fmt.Sprintf("languages.exclude_paths: %s has an empty prefix", repo))
			continue
		}
		if !anyFileUnder(files, trimmed) {
			warns = append(warns, fmt.Sprintf(
				"languages.exclude_paths: %s prefix %q matches no file", repo, prefix))
		}
	}
	return warns
}

func anyFileUnder(files []gh.TreeFile, prefix string) bool {
	for _, f := range files {
		if matchesOnePrefix(f.Path, prefix) {
			return true
		}
	}
	return false
}

// pathSubtractions turns the fetched trees into per-repo, per-language byte
// counts to remove.
func pathSubtractions(data *gh.Data, cfg config.Languages) ([]Subtraction, map[string]map[string]int64) {
	// Config drives the subtraction, not the dump. A fixture captured while
	// path exclusions were configured still carries the trees; replaying it
	// after the config is cleared must not keep subtracting.
	if len(data.Trees) == 0 || (len(cfg.ExcludePaths) == 0 && len(cfg.GlobalExcludePaths) == 0) {
		return nil, nil
	}

	reported := map[string]map[string]int64{}
	for _, r := range data.Repositories {
		m := make(map[string]int64, len(r.Languages))
		for _, l := range r.Languages {
			m[strings.ToLower(l.Name)] = l.Size
		}
		reported[r.Name] = m
	}

	byRepo := map[string]map[string]int64{}
	var out []Subtraction
	for _, tree := range data.Trees {
		subs := treeSubtractions(tree, reported[tree.Repo])
		if len(subs) == 0 {
			continue
		}
		byRepo[tree.Repo] = make(map[string]int64, len(subs))
		for _, s := range subs {
			byRepo[tree.Repo][strings.ToLower(s.Language)] = s.Bytes
		}
		out = append(out, subs...)
	}
	return out, byRepo
}

// treeSubtractions sums one tree's counted files by language, capped at what
// GitHub reported for each language, in language key order.
func treeSubtractions(tree gh.RepoTree, reported map[string]int64) []Subtraction {
	bytesByLang := map[string]int64{}
	filesByLang := map[string]int{}
	display := map[string]string{}
	for _, f := range tree.Files {
		lang := countedLanguage(f)
		if lang == "" {
			continue
		}
		key := strings.ToLower(lang)
		bytesByLang[key] += f.Size
		filesByLang[key]++
		display[key] = lang
	}

	out := make([]Subtraction, 0, len(bytesByLang))
	for _, key := range sortedKeys(bytesByLang) {
		want := bytesByLang[key]
		have := reported[key]
		out = append(out, Subtraction{
			Repo:     tree.Repo,
			Language: display[key],
			Bytes:    min(want, have),
			Files:    filesByLang[key],
			Clamped:  want > have,
		})
	}
	return out
}

// countedLanguage is the language a file adds to GitHub's totals, or "" when
// it adds nothing.
func countedLanguage(f gh.TreeFile) string {
	lang := f.Language
	if lang == "" {
		lang = languageForPath(f.Path)
	}
	if lang == "" {
		// Not a language GitHub counts towards the bar.
		return ""
	}
	if isVendoredPath(f.Path) {
		// linguist already leaves these out of the totals, so subtracting
		// them would remove bytes that were never added.
		return ""
	}
	return lang
}

func matchesOnePrefix(path, prefix string) bool {
	prefix = strings.TrimPrefix(strings.TrimSpace(prefix), "/")
	if path == prefix {
		return true
	}
	return strings.HasPrefix(path, strings.TrimSuffix(prefix, "/")+"/")
}

// isVendoredPath covers the common paths GitHub's linguist treats as vendored
// and leaves out of a repository's language totals.
func isVendoredPath(path string) bool {
	lower := strings.ToLower(path)
	for _, dir := range []string{"node_modules/", "bower_components/", "vendor/", "third_party/"} {
		if strings.HasPrefix(lower, dir) || strings.Contains(lower, "/"+dir) {
			return true
		}
	}
	return strings.HasSuffix(lower, ".min.js") || strings.HasSuffix(lower, ".min.css")
}

// languageForPath maps a file extension to the language GitHub would credit.
//
// Only languages that count towards a repository's language bar are listed.
// Data and prose formats such as JSON, YAML, XML and Markdown are deliberately
// absent: linguist excludes them from the totals, so there is nothing to take
// away.
//
// .php maps to PHP here. A .php file with no "<?php" opening tag is actually
// counted as Hack; that case is resolved by reading the blob during the fetch,
// and the answer arrives on TreeFile.Language, which takes priority over this
// table.
var extLanguage = map[string]string{
	".php": "PHP", ".phtml": "PHP", ".php3": "PHP", ".php4": "PHP", ".php5": "PHP", ".inc": "PHP",
	".go": "Go", ".js": "JavaScript", ".mjs": "JavaScript", ".cjs": "JavaScript", ".jsx": "JavaScript",
	".ts": "TypeScript", ".tsx": "TypeScript", ".py": "Python", ".rb": "Ruby",
	".css": "CSS", ".scss": "SCSS", ".sass": "Sass", ".less": "Less", ".styl": "Stylus",
	".html": "HTML", ".htm": "HTML", ".twig": "Twig", ".hbs": "Handlebars", ".ejs": "EJS",
	".vue": "Vue", ".svelte": "Svelte", ".sh": "Shell", ".bash": "Shell", ".zsh": "Shell",
	".sql": "SQL", ".java": "Java", ".kt": "Kotlin", ".kts": "Kotlin", ".cs": "C#",
	".c": "C", ".h": "C", ".cpp": "C++", ".cc": "C++", ".cxx": "C++", ".hpp": "C++",
	".rs": "Rust", ".swift": "Swift", ".lua": "Lua", ".ex": "Elixir", ".exs": "Elixir",
	".erl": "Erlang", ".zig": "Zig", ".pl": "Perl", ".pm": "Perl", ".r": "R",
	".dart": "Dart", ".scala": "Scala", ".clj": "Clojure", ".hs": "Haskell",
	".tf": "HCL", ".hcl": "HCL", ".gd": "GDScript", ".ps1": "PowerShell",
	".shader": "ShaderLab", ".hlsl": "HLSL", ".rtf": "Rich Text Format",
}

func languageForPath(path string) string {
	i := strings.LastIndexByte(path, '.')
	if i < 0 || strings.ContainsAny(path[i:], "/") {
		return ""
	}
	return extLanguage[strings.ToLower(path[i:])]
}

// nearestRepo suggests a close repository name, which is how a typo like a
// stray double space gets spotted quickly.
func nearestRepo(name string, repos map[string]bool) string {
	squashed := strings.Join(strings.Fields(name), "")
	best, bestDist := "", 4
	for repo := range repos {
		if strings.EqualFold(strings.Join(strings.Fields(repo), ""), squashed) {
			return fmt.Sprintf(" (did you mean %q?)", repo)
		}
		if d := editDistance(strings.ToLower(name), strings.ToLower(repo)); d < bestDist {
			best, bestDist = repo, d
		}
	}
	if best != "" {
		return fmt.Sprintf(" (did you mean %q?)", best)
	}
	return ""
}

func editDistance(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(min(cur[j-1]+1, prev[j]+1), prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(rb)]
}

func lowerKeys(in map[string]int64) map[string]int64 {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]int64, len(in))
	for k, v := range in {
		out[strings.ToLower(strings.TrimSpace(k))] = v
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
