package stats

import (
	"fmt"
	"sort"
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
	res := StreakResult{Today: today}

	// start_year is applied here as well as at fetch time, so replaying a
	// dump that holds every year still honours the configured window.
	// Left at 0 every year GitHub reported is used, which is the point:
	// the account creation year is not the first year with contributions.
	for _, y := range data.ContributionYears {
		if cfg.StartYear == 0 || y >= cfg.StartYear {
			res.Years = append(res.Years, y)
		}
	}

	// GitHub pads each yearly calendar out to whole weeks, so the first and
	// last weeks of a year carry days that belong to the neighbouring year.
	// Consecutive years therefore overlap, and summing the calendars as
	// returned double-counts those days. Deduplicate by date.
	counts := make(map[string]int, 512)
	var duplicates int
	for _, cal := range data.Calendars {
		if cfg.StartYear > 0 && cal.Year < cfg.StartYear {
			continue
		}
		for _, day := range cal.Days {
			if existing, seen := counts[day.Date]; seen {
				duplicates++
				// Identical values are the normal overlap. A genuine
				// disagreement is worth surfacing.
				if existing != day.Count {
					res.Warnings = append(res.Warnings, fmt.Sprintf(
						"contribution calendars disagree for %s (%d and %d); keeping the larger",
						day.Date, existing, day.Count))
					if day.Count > existing {
						counts[day.Date] = day.Count
					}
				}
				continue
			}
			counts[day.Date] = day.Count
		}
	}

	dates := make([]string, 0, len(counts))
	for d := range counts {
		dates = append(dates, d)
	}
	sort.Strings(dates)
	res.DaysConsidered = len(dates)

	excluded := cfg.ExcludedWeekdays()

	current := Streak{Start: today, End: today}
	longest := Streak{Start: today, End: today}
	var firstContribution time.Time

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

		count := counts[ds]
		res.TotalContributions += count

		switch {
		case count > 0:
			current.Length++
			current.End = day
			if current.Length == 1 {
				current.Start = day
			}
			if firstContribution.IsZero() {
				firstContribution = day
			}
			if current.Length > longest.Length {
				longest = current
			}

		case excluded != nil && excluded[day.Weekday()]:
			// An excluded day extends a streak that is already running but
			// never starts one, and never breaks one.
			if current.Length > 0 {
				current.Length++
				current.End = day
				if current.Length > longest.Length {
					longest = current
				}
			}

		case day.Equal(today):
			// A day with no contributions yet must not break the streak.
			// Without this the card reads zero every morning until the first
			// commit of the day lands.

		default:
			current = Streak{Start: today, End: today, Length: 0}
		}
	}

	res.FirstContribution = firstContribution
	res.Current = current
	res.Longest = longest

	if len(data.Calendars) == 0 {
		res.Warnings = append(res.Warnings, "no contribution calendars were fetched")
	}
	if cfg.StartYear > 0 && len(data.ContributionYears) > 0 && cfg.StartYear > data.ContributionYears[0] {
		res.Warnings = append(res.Warnings, fmt.Sprintf(
			"streak.start_year is %d, which drops contribution years GitHub reported starting at %d",
			cfg.StartYear, data.ContributionYears[0]))
	}
	return res
}

func truncateDay(t time.Time, loc *time.Location) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, loc)
}
