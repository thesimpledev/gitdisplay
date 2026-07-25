package stats_test

import (
	"testing"
	"time"

	"github.com/thesimpledev/gitdisplay/internal/config"
	"github.com/thesimpledev/gitdisplay/internal/gh"
	"github.com/thesimpledev/gitdisplay/internal/stats"
)

// synth builds a calendar from a date -> count map.
func synth(year int, days map[string]int) gh.Calendar {
	cal := gh.Calendar{Year: year}
	for date, count := range days {
		cal.Days = append(cal.Days, gh.Day{Date: date, Count: count})
	}
	return cal
}

// testLocation is a fixed zone with a non-zero offset, so a bug that leaks
// the host's local time shows up instead of passing by luck.
func testLocation(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func runStreak(t *testing.T, data *gh.Data, cfg config.Streak, today string) stats.StreakResult {
	t.Helper()
	loc := testLocation(t)
	now, err := time.ParseInLocation("2006-01-02", today, loc)
	if err != nil {
		t.Fatal(err)
	}
	return stats.Streaks(data, cfg, loc, now)
}

// TestOverlappingCalendarsAreDeduplicated covers the week-boundary overlap.
//
// When a yearly window is padded out to whole weeks, the last days of one year
// arrive again in the next year's calendar. Summing the calendars as returned
// would count those days twice.
func TestOverlappingCalendarsAreDeduplicated(t *testing.T) {
	data := &gh.Data{
		ContributionYears: []int{2024, 2025},
		Calendars: []gh.Calendar{
			synth(2024, map[string]int{
				"2024-12-30": 5,
				"2024-12-31": 7,
			}),
			// 2025's calendar repeats the tail of 2024's final week.
			synth(2025, map[string]int{
				"2024-12-30": 5,
				"2024-12-31": 7,
				"2025-01-01": 3,
			}),
		},
	}

	res := runStreak(t, data, config.Default().Streak, "2025-01-02")

	if res.TotalContributions != 15 {
		t.Errorf("total: got %d, want 15 (double counting would give 27)", res.TotalContributions)
	}
	if res.DaysConsidered != 3 {
		t.Errorf("distinct days: got %d, want 3", res.DaysConsidered)
	}
}

// TestZeroContributionsTodayDoesNotBreakStreak is the rule that looks like a
// bug and is not. Without it the card reads zero every morning until the first
// commit of the day lands.
func TestZeroContributionsTodayDoesNotBreakStreak(t *testing.T) {
	data := &gh.Data{
		ContributionYears: []int{2026},
		Calendars: []gh.Calendar{synth(2026, map[string]int{
			"2026-07-20": 1,
			"2026-07-21": 2,
			"2026-07-22": 3,
			"2026-07-23": 0, // a real gap: this one does break the streak
			"2026-07-24": 4,
			"2026-07-25": 0, // today, nothing committed yet
		})},
	}

	res := runStreak(t, data, config.Default().Streak, "2026-07-25")

	if res.Current.Length != 1 {
		t.Errorf("current streak: got %d, want 1 (today's zero must not break it)", res.Current.Length)
	}
	if got := res.Current.End.Format("2006-01-02"); got != "2026-07-24" {
		t.Errorf("current streak should end on the last day with contributions, got %s", got)
	}
	if res.Longest.Length != 3 {
		t.Errorf("longest streak: got %d, want 3", res.Longest.Length)
	}
}

// TestYesterdayZeroDoesBreakStreak confirms the exception is only for today.
func TestYesterdayZeroDoesBreakStreak(t *testing.T) {
	data := &gh.Data{
		ContributionYears: []int{2026},
		Calendars: []gh.Calendar{synth(2026, map[string]int{
			"2026-07-22": 3,
			"2026-07-23": 4,
			"2026-07-24": 0, // yesterday, nothing: this breaks it
			"2026-07-25": 0, // today
		})},
	}

	res := runStreak(t, data, config.Default().Streak, "2026-07-25")

	if res.Current.Length != 0 {
		t.Errorf("current streak: got %d, want 0", res.Current.Length)
	}
}

// TestExcludedDayExtendsButNeverStarts covers the other rule that looks wrong
// and is not.
func TestExcludedDayExtendsButNeverStarts(t *testing.T) {
	cfg := config.Default().Streak
	// 2026-07-18 is a Saturday, 2026-07-19 a Sunday.
	cfg.ExcludeDays = []string{"Sat", "Sun"}

	t.Run("extends a running streak", func(t *testing.T) {
		data := &gh.Data{
			ContributionYears: []int{2026},
			Calendars: []gh.Calendar{synth(2026, map[string]int{
				"2026-07-16": 1, // Thu
				"2026-07-17": 2, // Fri
				"2026-07-18": 0, // Sat, excluded
				"2026-07-19": 0, // Sun, excluded
				"2026-07-20": 3, // Mon
			})},
		}
		res := runStreak(t, data, cfg, "2026-07-20")
		if res.Current.Length != 5 {
			t.Errorf("got %d, want 5: the weekend should extend the streak", res.Current.Length)
		}
		if got := res.Current.Start.Format("2006-01-02"); got != "2026-07-16" {
			t.Errorf("start: got %s, want 2026-07-16", got)
		}
	})

	t.Run("never starts one", func(t *testing.T) {
		data := &gh.Data{
			ContributionYears: []int{2026},
			Calendars: []gh.Calendar{synth(2026, map[string]int{
				"2026-07-17": 0, // Fri, a real gap
				"2026-07-18": 0, // Sat, excluded, but nothing is running
				"2026-07-19": 0, // Sun, excluded
				"2026-07-20": 5, // Mon, this starts the streak
			})},
		}
		res := runStreak(t, data, cfg, "2026-07-20")
		if res.Current.Length != 1 {
			t.Errorf("got %d, want 1: an excluded day must not start a streak", res.Current.Length)
		}
		if got := res.Current.Start.Format("2006-01-02"); got != "2026-07-20" {
			t.Errorf("start: got %s, want 2026-07-20", got)
		}
	})
}

// TestFutureDaysAreIgnored guards the current streak against the empty days
// that the rest of the current year is made of.
func TestFutureDaysAreIgnored(t *testing.T) {
	data := &gh.Data{
		ContributionYears: []int{2026},
		Calendars: []gh.Calendar{synth(2026, map[string]int{
			"2026-07-24": 4,
			"2026-07-25": 2,
			"2026-07-26": 0, // tomorrow
			"2026-12-31": 0,
		})},
	}

	res := runStreak(t, data, config.Default().Streak, "2026-07-25")

	if res.Current.Length != 2 {
		t.Errorf("current streak: got %d, want 2", res.Current.Length)
	}
	if res.TotalContributions != 6 {
		t.Errorf("total: got %d, want 6", res.TotalContributions)
	}
}

// TestStartYearZeroUsesEveryReportedYear is the contribution-years fix in
// isolation: the account creation year must not be the floor.
func TestStartYearZeroUsesEveryReportedYear(t *testing.T) {
	data := &gh.Data{
		ContributionYears: []int{2013, 2014},
		Calendars: []gh.Calendar{
			synth(2013, map[string]int{"2013-02-17": 4}),
			synth(2014, map[string]int{"2014-04-01": 1}),
		},
	}

	full := runStreak(t, data, config.Default().Streak, "2026-07-25")
	if full.TotalContributions != 5 {
		t.Errorf("total: got %d, want 5", full.TotalContributions)
	}
	if got := full.FirstContribution.Format("2006-01-02"); got != "2013-02-17" {
		t.Errorf("first contribution: got %s, want 2013-02-17", got)
	}

	clamped := config.Default().Streak
	clamped.StartYear = 2014
	res := runStreak(t, data, clamped, "2026-07-25")
	if res.TotalContributions != 1 {
		t.Errorf("clamped total: got %d, want 1", res.TotalContributions)
	}
	if len(res.Warnings) == 0 {
		t.Error("dropping reported contribution years should warn")
	}
}
