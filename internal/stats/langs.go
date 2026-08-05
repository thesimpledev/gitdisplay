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

	// An exclusion matches either the bare name or the qualified owner/name.
	excluded := make(map[string]bool, len(cfg.ExcludeRepos))
	for _, name := range cfg.ExcludeRepos {
		excluded[name] = true
	}
	isExcluded := func(r gh.Repo) bool {
		return excluded[r.Name] || (r.NameWithOwner != "" && excluded[r.NameWithOwner])
	}

	subtractions, subByRepo := pathSubtractions(data, cfg)
	res.Subtractions = subtractions

	// Aliases merge one language into another before ranking, so the SQL
	// dialects GitHub reports separately can share a row.
	aliases := make(map[string]string, len(cfg.LanguageAliases))
	for from, to := range cfg.LanguageAliases {
		aliases[strings.ToLower(strings.TrimSpace(from))] = strings.TrimSpace(to)
	}

	hidden := make(map[string]bool, len(cfg.HideLanguages))
	for _, name := range cfg.HideLanguages {
		hidden[strings.ToLower(strings.TrimSpace(name))] = true
	}

	// Language name -> accumulated bytes. Names are kept in the casing GitHub
	// uses; lookups are case-insensitive.
	type acc struct {
		name  string
		color string
		// largest is the biggest single contribution seen, so a merged row
		// takes the colour of whichever source dominates it.
		largest int64
		size    int64
	}
	totals := map[string]*acc{}
	seenLangs := map[string]bool{}

	for _, repo := range repos {
		// Record which languages exist anywhere in the fetched data before
		// any filtering. A hide entry for a language that only appears in
		// archived repositories is still doing its job, so it must not be
		// reported as matching nothing.
		for _, lang := range repo.Languages {
			seenLangs[strings.ToLower(lang.Name)] = true
		}

		if isExcluded(repo) {
			continue
		}
		if !include(repo) {
			if repo.Archived {
				res.ArchivedSkipped = append(res.ArchivedSkipped, repo.Name)
			}
			continue
		}

		langs := repo.Languages
		// LanguagesPerRepo reproduces the original's languages(first: 10),
		// which lost the tail. The API returns them largest first.
		if cfg.LanguagesPerRepo > 0 && len(langs) > cfg.LanguagesPerRepo {
			langs = langs[:cfg.LanguagesPerRepo]
		}
		if len(langs) == 0 {
			continue
		}
		res.ReposConsidered++

		overrides := lowerKeys(cfg.ByteOverrides[repo.Name])
		subs := subByRepo[repo.Name]

		for _, lang := range langs {
			key := strings.ToLower(lang.Name)
			seenLangs[key] = true

			// Resolve the alias, then hide on either the reported name or
			// the name it merged into.
			display := lang.Name
			if to, ok := aliases[key]; ok && to != "" {
				display = to
			}
			if hidden[key] || hidden[strings.ToLower(display)] {
				continue
			}

			size := lang.Size
			if sub, ok := subs[key]; ok {
				size -= sub
				if size < 0 {
					size = 0
				}
			}
			// A manual override is the exact escape hatch and wins over any
			// computed path subtraction.
			if v, ok := overrides[key]; ok {
				size = v
			}
			if size <= 0 {
				continue
			}

			mergedKey := strings.ToLower(display)
			a := totals[mergedKey]
			if a == nil {
				a = &acc{name: display}
				totals[mergedKey] = a
			}
			if size > a.largest {
				a.largest, a.color = size, lang.Color
			}
			a.size += size
		}
	}

	// Hiding happens before the top-N cut, so a hidden language never uses up
	// one of the displayed slots.
	visible := make([]LangStat, 0, len(totals))
	var visibleBytes int64
	for key, a := range totals {
		if hidden[key] {
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

	if cfg.Count > 0 && len(visible) > cfg.Count {
		visible = visible[:cfg.Count]
	}

	var displayed int64
	for _, l := range visible {
		displayed += l.Size
	}

	// The denominator is the sum of the displayed languages, so the card adds
	// up to 100%. Set percent_of_displayed to false to divide by every
	// non-hidden language instead.
	denom := displayed
	if !cfg.PercentOfDisplayed {
		denom = visibleBytes
	}
	if denom > 0 {
		for i := range visible {
			visible[i].Percent = float64(visible[i].Size) / float64(denom) * 100
		}
	}

	res.Languages = visible
	res.DisplayedBytes = denom
	if withWarnings {
		res.Warnings = validateLanguages(data, cfg, repos, seenLangs)
	}
	return res
}

// validateLanguages reports every configured entry that matched nothing.
//
// A silent no-op here is how a typo disables an exclusion without anyone
// noticing, so each list is checked against the data actually fetched.
func validateLanguages(data *gh.Data, cfg config.Languages, repos []gh.Repo, seenLangs map[string]bool) []string {
	var warns []string

	repoNames := make(map[string]bool, len(repos))
	for _, r := range repos {
		repoNames[r.Name] = true
	}
	// Repositories beyond MaxRepos still exist; naming one is not a typo.
	allRepoNames := make(map[string]bool, len(data.Repositories)*2)
	for _, r := range data.Repositories {
		allRepoNames[r.Name] = true
		if r.NameWithOwner != "" {
			allRepoNames[r.NameWithOwner] = true
		}
	}

	archived := make(map[string]bool)
	for _, r := range data.Repositories {
		if r.Archived {
			archived[r.Name] = true
		}
	}

	for _, name := range cfg.ExcludeRepos {
		switch {
		case strings.TrimSpace(name) == "":
			warns = append(warns, "languages.exclude_repos: empty entry")
		case allRepoNames[name]:
			// Named a real repository. Fine even if MaxRepos cut it off.
			// Point out entries that exclude_archived already covers, so the
			// list can shrink to the repositories archiving does not handle.
			if cfg.ExcludeArchived && archived[name] {
				warns = append(warns, fmt.Sprintf(
					"languages.exclude_repos: %q is archived and already excluded; the entry is redundant", name))
			}
		default:
			warns = append(warns, fmt.Sprintf(
				"languages.exclude_repos: %q matches no repository%s", name, nearestRepo(name, allRepoNames)))
		}
	}

	for _, name := range cfg.HideLanguages {
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

	langsByRepo := map[string]map[string]bool{}
	for _, r := range data.Repositories {
		set := make(map[string]bool, len(r.Languages))
		for _, l := range r.Languages {
			set[strings.ToLower(l.Name)] = true
		}
		langsByRepo[r.Name] = set
	}

	for _, repo := range sortedKeys(cfg.ByteOverrides) {
		if !allRepoNames[repo] {
			warns = append(warns, fmt.Sprintf(
				"languages.byte_overrides: repository %q does not exist%s", repo, nearestRepo(repo, allRepoNames)))
			continue
		}
		for _, lang := range sortedKeys(cfg.ByteOverrides[repo]) {
			if !langsByRepo[repo][strings.ToLower(strings.TrimSpace(lang))] {
				warns = append(warns, fmt.Sprintf(
					"languages.byte_overrides: %s has no language %q", repo, lang))
			}
		}
	}

	treeByRepo := map[string]*gh.RepoTree{}
	for i := range data.Trees {
		treeByRepo[data.Trees[i].Repo] = &data.Trees[i]
	}
	for _, repo := range sortedKeys(cfg.ExcludePaths) {
		if !allRepoNames[repo] {
			warns = append(warns, fmt.Sprintf(
				"languages.exclude_paths: repository %q does not exist%s", repo, nearestRepo(repo, allRepoNames)))
			continue
		}
		tree := treeByRepo[repo]
		if tree == nil {
			warns = append(warns, fmt.Sprintf(
				"languages.exclude_paths: %s was not read; no paths were subtracted", repo))
			continue
		}
		if tree.Truncated {
			warns = append(warns, fmt.Sprintf(
				"languages.exclude_paths: %s tree was truncated by the API; subtraction is incomplete", repo))
		}
		for _, prefix := range cfg.ExcludePaths[repo] {
			trimmed := strings.TrimSpace(prefix)
			if trimmed == "" {
				warns = append(warns, fmt.Sprintf("languages.exclude_paths: %s has an empty prefix", repo))
				continue
			}
			matched := false
			for _, f := range tree.Files {
				if matchesOnePrefix(f.Path, trimmed) {
					matched = true
					break
				}
			}
			if !matched {
				warns = append(warns, fmt.Sprintf(
					"languages.exclude_paths: %s prefix %q matches no file", repo, prefix))
			}
		}
	}

	return warns
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
		bytesByLang := map[string]int64{}
		filesByLang := map[string]int{}
		display := map[string]string{}

		for _, f := range tree.Files {
			lang := f.Language
			if lang == "" {
				lang = languageForPath(f.Path)
			}
			if lang == "" {
				// Not a language GitHub counts towards the bar.
				continue
			}
			if isVendoredPath(f.Path) {
				// linguist already leaves these out of the totals, so
				// subtracting them would remove bytes that were never added.
				continue
			}
			key := strings.ToLower(lang)
			bytesByLang[key] += f.Size
			filesByLang[key]++
			display[key] = lang
		}

		if len(bytesByLang) == 0 {
			continue
		}
		byRepo[tree.Repo] = map[string]int64{}
		for _, key := range sortedKeys(bytesByLang) {
			want := bytesByLang[key]
			have := reported[tree.Repo][key]
			clamped := false
			if want > have {
				want = have
				clamped = true
			}
			byRepo[tree.Repo][key] = want
			out = append(out, Subtraction{
				Repo:     tree.Repo,
				Language: display[key],
				Bytes:    want,
				Files:    filesByLang[key],
				Clamped:  clamped,
			})
		}
	}
	return out, byRepo
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
