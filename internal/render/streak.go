package render

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/thesimpledev/gitdisplay/internal/config"
	"github.com/thesimpledev/gitdisplay/internal/stats"
)

const (
	streakWidth  = 495
	streakHeight = 195
)

// Streak renders the contribution streak card.
func Streak(res stats.StreakResult, cfg config.Streak) []byte {
	t := cfg.Theme

	var b strings.Builder
	b.Grow(4096)

	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" xmlns:xlink="http://www.w3.org/1999/xlink" `+
		`style="isolation: isolate" viewBox="0 0 %d %d" width="%dpx" height="%dpx" direction="ltr" `+
		`role="img" aria-labelledby="descId">`,
		streakWidth, streakHeight, streakWidth, streakHeight)

	fmt.Fprintf(&b, `<title id="titleId">GitHub contribution streak</title>`)
	fmt.Fprintf(&b, `<desc id="descId">%s</desc>`, xmlEscape(streakDescription(res)))

	b.WriteString(`<style>
@keyframes currstreak { 0% { font-size: 3px; opacity: 0.2; } 80% { font-size: 34px; opacity: 1; } 100% { font-size: 28px; opacity: 1; } }
@keyframes fadein { 0% { opacity: 0; } 100% { opacity: 1; } }
</style>`)

	fmt.Fprintf(&b, `<defs><clipPath id="outer_rectangle"><rect width="%d" height="%d" rx="4.5"/></clipPath>`+
		`<mask id="mask_out_ring_behind_fire"><rect width="%d" height="%d" fill="white"/>`+
		`<ellipse id="mask-ellipse" cx="247.5" cy="32" rx="13" ry="18" fill="black"/></mask></defs>`,
		streakWidth, streakHeight, streakWidth, streakHeight)

	b.WriteString(`<g clip-path="url(#outer_rectangle)">`)

	border := t.Border
	if t.HideBorder {
		border = "none"
	}
	fmt.Fprintf(&b, `<g style="isolation: isolate">`+
		`<rect stroke="%s" fill="%s" rx="4.5" x="0.5" y="0.5" width="%d" height="%d"/></g>`,
		border, t.Background, streakWidth-1, streakHeight-1)

	fmt.Fprintf(&b, `<g style="isolation: isolate">`+
		`<line x1="165" y1="28" x2="165" y2="170" vector-effect="non-scaling-stroke" stroke-width="1" stroke="%s" stroke-linejoin="miter" stroke-linecap="square" stroke-miterlimit="3"/>`+
		`<line x1="330" y1="28" x2="330" y2="170" vector-effect="non-scaling-stroke" stroke-width="1" stroke="%s" stroke-linejoin="miter" stroke-linecap="square" stroke-miterlimit="3"/></g>`,
		t.Divider, t.Divider)

	today := res.Today

	// Panel 1: total contributions.
	b.WriteString(`<g style="isolation: isolate">`)
	streakBigNumber(&b, 82.5, t.Text, formatThousands(res.TotalContributions),
		`opacity: 0; animation: fadein 0.5s linear forwards 0.6s`)
	streakLabel(&b, 82.5, 84, t.Label, "Total Contributions", 0.7)
	streakDates(&b, 82.5, 114, t.Dates, totalRange(res, today))
	b.WriteString(`</g>`)

	// Panel 2: current streak.
	b.WriteString(`<g style="isolation: isolate">`)
	fmt.Fprintf(&b, `<g transform="translate(247.5, 108)">`+
		`<text x="0" y="32" stroke-width="0" text-anchor="middle" fill="%s" stroke="none" `+
		`font-family="&quot;Segoe UI&quot;, Ubuntu, sans-serif" font-weight="700" font-size="14px" `+
		`font-style="normal" style="opacity: 0; animation: fadein 0.5s linear forwards 0.9s">Current Streak</text></g>`,
		t.Accent)
	fmt.Fprintf(&b, `<g transform="translate(247.5, 145)">`+
		`<text x="0" y="21" stroke-width="0" text-anchor="middle" fill="%s" stroke="none" `+
		`font-family="&quot;Segoe UI&quot;, Ubuntu, sans-serif" font-weight="400" font-size="12px" `+
		`font-style="normal" style="opacity: 0; animation: fadein 0.5s linear forwards 0.9s">%s</text></g>`,
		t.Dates, xmlEscape(streakRange(res.Current, today)))
	fmt.Fprintf(&b, `<g mask="url(#mask_out_ring_behind_fire)">`+
		`<circle cx="247.5" cy="71" r="40" fill="none" stroke="%s" stroke-width="5" `+
		`style="opacity: 0; animation: fadein 0.5s linear forwards 0.4s"></circle></g>`, t.Accent)
	fmt.Fprintf(&b, `<g transform="translate(247.5, 19.5)" stroke-opacity="0" `+
		`style="opacity: 0; animation: fadein 0.5s linear forwards 0.6s">`+
		`<path d="M -12 -0.5 L 15 -0.5 L 15 23.5 L -12 23.5 L -12 -0.5 Z" fill="none"/>`+
		`<path d="M 1.5 0.67 C 1.5 0.67 2.24 3.32 2.24 5.47 C 2.24 7.53 0.89 9.2 -1.17 9.2 C -3.23 9.2 -4.79 7.53 -4.79 5.47 L -4.76 5.11 C -6.78 7.51 -8 10.62 -8 13.99 C -8 18.41 -4.42 22 0 22 C 4.42 22 8 18.41 8 13.99 C 8 8.6 5.41 3.79 1.5 0.67 Z M -0.29 19 C -2.07 19 -3.51 17.6 -3.51 15.86 C -3.51 14.24 -2.46 13.1 -0.7 12.74 C 1.07 12.38 2.9 11.53 3.92 10.16 C 4.31 11.45 4.51 12.81 4.51 14.2 C 4.51 16.85 2.36 19 -0.29 19 Z" fill="%s" stroke-opacity="0"/></g>`,
		t.Accent)
	fmt.Fprintf(&b, `<g transform="translate(247.5, 48)">`+
		`<text x="0" y="32" stroke-width="0" text-anchor="middle" fill="%s" stroke="none" `+
		`font-family="&quot;Segoe UI&quot;, Ubuntu, sans-serif" font-weight="700" font-size="28px" `+
		`font-style="normal" style="animation: currstreak 0.6s linear forwards">%s</text></g>`,
		t.Text, formatThousands(res.Current.Length))
	b.WriteString(`</g>`)

	// Panel 3: longest streak.
	b.WriteString(`<g style="isolation: isolate">`)
	streakBigNumber(&b, 412.5, t.Text, formatThousands(res.Longest.Length),
		`opacity: 0; animation: fadein 0.5s linear forwards 1.2s`)
	streakLabel(&b, 412.5, 84, t.Label, "Longest Streak", 1.3)
	streakDates(&b, 412.5, 114, t.Dates, streakRange(res.Longest, today))
	b.WriteString(`</g>`)

	b.WriteString(`</g></svg>`)
	return []byte(b.String())
}

func streakBigNumber(b *strings.Builder, x float64, fill, text, style string) {
	fmt.Fprintf(b, `<g transform="translate(%s, 48)">`+
		`<text x="0" y="32" stroke-width="0" text-anchor="middle" fill="%s" stroke="none" `+
		`font-family="&quot;Segoe UI&quot;, Ubuntu, sans-serif" font-weight="700" font-size="28px" `+
		`font-style="normal" style="%s">%s</text></g>`,
		trimFloat(x), fill, style, xmlEscape(text))
}

func streakLabel(b *strings.Builder, x float64, y int, fill, text string, delay float64) {
	fmt.Fprintf(b, `<g transform="translate(%s, %d)">`+
		`<text x="0" y="32" stroke-width="0" text-anchor="middle" fill="%s" stroke="none" `+
		`font-family="&quot;Segoe UI&quot;, Ubuntu, sans-serif" font-weight="400" font-size="14px" `+
		`font-style="normal" style="opacity: 0; animation: fadein 0.5s linear forwards %ss">%s</text></g>`,
		trimFloat(x), y, fill, trimFloat(delay), xmlEscape(text))
}

func streakDates(b *strings.Builder, x float64, y int, fill, text string) {
	delay := "0.8"
	if y == 114 && x > 300 {
		delay = "1.4"
	}
	fmt.Fprintf(b, `<g transform="translate(%s, %d)">`+
		`<text x="0" y="32" stroke-width="0" text-anchor="middle" fill="%s" stroke="none" `+
		`font-family="&quot;Segoe UI&quot;, Ubuntu, sans-serif" font-weight="400" font-size="12px" `+
		`font-style="normal" style="opacity: 0; animation: fadein 0.5s linear forwards %ss">%s</text></g>`,
		trimFloat(x), y, fill, delay, xmlEscape(text))
}

// totalRange is the span the total covers: first contribution through today.
func totalRange(res stats.StreakResult, today time.Time) string {
	if res.FirstContribution.IsZero() {
		return formatCardDate(today, today)
	}
	return formatCardDate(res.FirstContribution, today) + " - Present"
}

// streakRange formats a streak's start and end. A streak of zero shows the
// current date, matching the card this replaces.
func streakRange(s stats.Streak, today time.Time) string {
	if s.Length == 0 {
		return formatCardDate(today, today)
	}
	start := formatCardDate(s.Start, today)
	if s.Start.Equal(s.End) {
		return start
	}
	return start + " - " + formatCardDate(s.End, today)
}

// formatCardDate prints "Jul 13" for dates in the current year and
// "Mar 30, 2024" otherwise.
func formatCardDate(d, today time.Time) string {
	if d.Year() == today.Year() {
		return d.Format("Jan 2")
	}
	return d.Format("Jan 2, 2006")
}

func streakDescription(res stats.StreakResult) string {
	return fmt.Sprintf("%s total contributions, current streak %d days, longest streak %d days",
		formatThousands(res.TotalContributions), res.Current.Length, res.Longest.Length)
}

// formatThousands renders 9878 as "9,878".
func formatThousands(n int) string {
	s := strconv.Itoa(n)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	if len(s) <= 3 {
		if neg {
			return "-" + s
		}
		return s
	}
	var out strings.Builder
	lead := len(s) % 3
	if lead > 0 {
		out.WriteString(s[:lead])
	}
	for i := lead; i < len(s); i += 3 {
		if out.Len() > 0 {
			out.WriteByte(',')
		}
		out.WriteString(s[i : i+3])
	}
	if neg {
		return "-" + out.String()
	}
	return out.String()
}

func trimFloat(f float64) string {
	s := strconv.FormatFloat(f, 'f', -1, 64)
	return s
}
