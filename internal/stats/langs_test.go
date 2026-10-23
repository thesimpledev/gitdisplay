package stats_test

import (
	"testing"

	"github.com/thesimpledev/gitdisplay/internal/config"
	"github.com/thesimpledev/gitdisplay/internal/gh"
	"github.com/thesimpledev/gitdisplay/internal/stats"
)

func repo(name string, langs ...gh.Lang) gh.Repo {
	return gh.Repo{Name: name, Owner: "example-user", NameWithOwner: "example-user/" + name, Languages: langs}
}

// TestEqualSizesRankByName keeps the card order stable when two languages
// carry the same number of bytes.
func TestEqualSizesRankByName(t *testing.T) {
	data := &gh.Data{Repositories: []gh.Repo{
		repo("some-repo", gh.Lang{Name: "Zig", Size: 100}, gh.Lang{Name: "Ada", Size: 100}),
	}}
	res := stats.Languages(data, config.Default().Languages)
	if len(res.Languages) != 2 || res.Languages[0].Name != "Ada" || res.Languages[1].Name != "Zig" {
		t.Fatalf("got %+v, want Ada then Zig", res.Languages)
	}
	if res.Languages[0].Percent != 50 {
		t.Errorf("percent: got %v, want 50", res.Languages[0].Percent)
	}
}

// TestTreesWithoutExclusionConfigSubtractNothing replays a dump captured
// while path exclusions were configured, after the config was cleared.
func TestTreesWithoutExclusionConfigSubtractNothing(t *testing.T) {
	data := &gh.Data{
		Repositories: []gh.Repo{repo("some-repo", gh.Lang{Name: "Go", Size: 1000})},
		Trees: []gh.RepoTree{{
			Repo:  "some-repo",
			Files: []gh.TreeFile{{Path: "gen/out.go", Size: 600}},
		}},
	}
	res := stats.Languages(data, config.Default().Languages)
	if len(res.Subtractions) != 0 {
		t.Errorf("subtractions: got %+v, want none", res.Subtractions)
	}
	if res.Languages[0].Size != 1000 {
		t.Errorf("Go bytes: got %d, want 1000", res.Languages[0].Size)
	}
}

// TestTreeWithOnlyUncountedFilesSubtractsNothing covers a prefix that holds
// only data files, which linguist never counted.
func TestTreeWithOnlyUncountedFilesSubtractsNothing(t *testing.T) {
	cfg := config.Default().Languages
	cfg.ExcludePaths = map[string][]string{"some-repo": {"docs/"}}
	data := &gh.Data{
		Repositories: []gh.Repo{repo("some-repo", gh.Lang{Name: "Go", Size: 1000})},
		Trees: []gh.RepoTree{{
			Repo:     "some-repo",
			Prefixes: []string{"docs/"},
			Files: []gh.TreeFile{
				{Path: "docs/guide.md", Size: 600},
				{Path: "docs/Makefile", Size: 50},
			},
		}},
	}
	res := stats.Languages(data, cfg)
	if len(res.Subtractions) != 0 {
		t.Errorf("subtractions: got %+v, want none", res.Subtractions)
	}
	if res.Languages[0].Size != 1000 {
		t.Errorf("Go bytes: got %d, want 1000", res.Languages[0].Size)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("warnings: got %q, want none", res.Warnings)
	}
}
