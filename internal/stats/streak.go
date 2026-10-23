package stats

import (
	"fmt"
	"time"

	"github.com/thesimpledev/gitdisplay/internal/config"
	"github.com/thesimpledev/gitdisplay/internal/gh"
)

// Streak is one contiguous run of contribution days.
type Streak struct {
	Start  time.Time `json:"start"`
	End    time.Time `json:"end"`
	Length int       `json:"length"`
}

// StreakResult is the finished streak computation.
type StreakResult struct {
	TotalContributions int       `json:"total_contributions"`
	FirstContribution  time.Time `json:"first_contribution"`
	Current            Streak    `json:"current_streak"`
	Longest            Streak    `json:"longest_streak"`

	// Today is the reference date all of this was computed against, in the
	// configured timezone.
	Today time.Time `json:"today"`
	// Years lists the contribution years actually used.
	Years []int `json:"years"`
	// DaysConsidered is the number of distinct dates after deduplication.
	DaysConsidered int      `json:"days_considered"`
	Warnings       []string `json:"warnings,omitempty"`
}

const dateLayout = "2006-01-02"

// Streaks computes totals and streaks from the fetched contribution calendars.
//
// now is the reference time; only its date in loc matters.
func Streaks(data *gh.Data, cfg config.Streak, loc *time.Location, now time.Time) StreakResult {
	today := truncateDay(now.In(loc), loc)
	res := StreakResult{Today: today, Years: yearsFrom(data.ContributionYears, cfg.StartYear)}

	counts, warnings := dedupeDays(data.Calendars, cfg.StartYear)
	res.Warnings = warnings

	dates := sortedKeys(counts)
	res.DaysConsidered = len(dates)

	w := streakWalker{
		today:    today,
		excluded: cfg.ExcludedWeekdays(),
		current:  Streak{Start: today, End: today},
		longest:  Streak{Start: today, End: today},
	}
	for _, ds := range dates {
		day, err := time.ParseInLocation(dateLayout, ds, loc)
		if err != nil {
			continue
		}
		// Days after today carry no contributions yet. Walking into them
		// would reset the current streak to zero every time.
		if day.After(today) {
			break
		}
		w.step(day, counts[ds])
	}

	res.TotalContributions = w.total
	res.FirstContribution = w.first
	res.Current = w.current
	res.Longest = w.longest
	res.Warnings = append(res.Warnings, streakConfigWarnings(data, cfg)...)
	return res
}

// yearsFrom applies start_year here as well as at fetch time, so replaying
// a dump that holds every year still honours the configured window. Left at
// 0 every year GitHub reported is used, which is the point: the account
// creation year is not the first year with contributions.
func yearsFrom(years []int, startYear int) []int {
	var out []int
	for _, y := range years {
		if startYear == 0 || y >= startYear {
			out = append(out, y)
		}
	}
	return out
}

// dedupeDays merges the yearly calendars into one count per date.
//
// GitHub pads each yearly calendar out to whole weeks, so the first and last
// weeks of a year carry days that belong to the neighbouring year.
// Consecutive years therefore overlap, and summing the calendars as returned
// double-counts those days.
func dedupeDays(calendars []gh.Calendar, startYear int) (map[string]int, []string) {
	counts := make(map[string]int, 512)
	var warnings []string
	for _, cal := range calendars {
		if startYear > 0 && cal.Year < startYear {
			continue
		}
		for _, day := range cal.Days {
			existing, seen := counts[day.Date]
			if !seen {
				counts[day.Date] = day.Count
				continue
			}
			// Identical values are the normal overlap. A genuine
			// disagreement is worth surfacing.
			if existing == day.Count {
				continue
			}
			warnings = append(warnings, fmt.Sprintf(
				"contribution calendars disagree for %s (%d and %d); keeping the larger",
				day.Date, existing, day.Count))
			counts[day.Date] = max(existing, day.Count)
		}
	}
	return counts, warnings
}

// streakWalker applies the streak rules one day at a time, in date order.
type streakWalker struct {
	today    time.Time
	excluded map[time.Weekday]bool
	total    int
	first    time.Time
	current  Streak
	longest  Streak
}

func (w *streakWalker) step(day time.Time, count int) {
	w.total += count
	switch {
	case count > 0:
		if w.first.IsZero() {
			w.first = day
		}
		if w.current.Length == 0 {
			w.current.Start = day
		}
		w.extend(day)

	case w.excluded[day.Weekday()]:
		// An excluded day extends a streak that is already running but
		// never starts one, and never breaks one.
		if w.current.Length > 0 {
			w.extend(day)
		}

	case day.Equal(w.today):
		// A day with no contributions yet must not break the streak.
		// Without this the card reads zero every morning until the first
		// commit of the day lands.

	default:
		w.current = Streak{Start: w.today, End: w.today, Length: 0}
	}
}

func (w *streakWalker) extend(day time.Time) {
	w.current.Length++
	w.current.End = day
	if w.current.Length > w.longest.Length {
		w.longest = w.current
	}
}

func streakConfigWarnings(data *gh.Data, cfg config.Streak) []string {
	var warnings []string
	if len(data.Calendars) == 0 {
		warnings = append(warnings, "no contribution calendars were fetched")
	}
	if cfg.StartYear > 0 && len(data.ContributionYears) > 0 && cfg.StartYear > data.ContributionYears[0] {
		warnings = append(warnings, fmt.Sprintf(
			"streak.start_year is %d, which drops contribution years GitHub reported starting at %d",
			cfg.StartYear, data.ContributionYears[0]))
	}
	return warnings
}

func truncateDay(t time.Time, loc *time.Location) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, loc)
}
