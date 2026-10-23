// Package render turns computed stats into SVG bytes.
//
// The output matches the geometry, class names and animation timings of the
// cards these replace, so swapping the URL in a README changes nothing
// visible.
package render

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/thesimpledev/gitdisplay/internal/config"
	"github.com/thesimpledev/gitdisplay/internal/stats"
)

const (
	langCardWidth = 300
	langRowHeight = 40
	langBarWidth  = 205
)

// Split-card geometry. One header, two columns side by side.
const (
	splitColumnWidth = 265 // a row is the bar plus its percentage text
	splitColumnGap   = 40
	splitCardWidth   = 25 + splitColumnWidth + splitColumnGap + splitColumnWidth + 25
	splitSectionHead = 26
)

// LangsSplit renders the two-section card: active repositories on the left,
// archived on the right, under a single header.
//
// Side by side rather than stacked so the two lists line up row for row and
// can be read against each other at a glance.
func LangsSplit(split stats.SplitResult, cfg config.Languages) []byte {
	theme := cfg.Theme
	active, archived := split.Active.Languages, split.Archived.Languages

	rows := max(len(active), len(archived))
	height := 55 + splitSectionHead + rows*langRowHeight + 12

	var b strings.Builder
	b.Grow(1600 + (len(active)+len(archived))*700)

	writeLangHead(&b, cfg, cardHead{
		width:    splitCardWidth,
		height:   height,
		desc:     langsDescription(append(append([]stats.LangStat{}, active...), archived...)),
		centered: true,
	})

	b.WriteString(`<g data-testid="main-card-body" transform="translate(0, 55)">`)

	// A divider between the columns, matching the streak card's separators.
	if len(active) > 0 && len(archived) > 0 {
		divX := 25 + splitColumnWidth + splitColumnGap/2
		fmt.Fprintf(&b, `<line x1="%d" y1="0" x2="%d" y2="%d" stroke="%s" `+
			`stroke-opacity="0.35" stroke-width="1" vector-effect="non-scaling-stroke"/>`,
			divX, divX, splitSectionHead+rows*langRowHeight, theme.Border)
	}

	// The two columns stagger together, so a row and its counterpart appear
	// at the same moment rather than the right column trailing the left.
	writeSplitColumn(&b, active, theme, 25, cfg.ActiveLabel)
	writeSplitColumn(&b, archived, theme, 25+splitColumnWidth+splitColumnGap, cfg.ArchivedLabel)

	b.WriteString(`</g></svg>`)
	return []byte(b.String())
}

func writeSplitColumn(b *strings.Builder, langs []stats.LangStat, theme config.Theme, x int, label string) {
	if len(langs) == 0 {
		return
	}
	fmt.Fprintf(b, `<svg data-testid="lang-items" x="%d">`, x)
	writeSectionLabel(b, 0, theme.Title, label)
	writeLangRows(b, langs, theme, splitSectionHead, 0)
	b.WriteString(`</svg>`)
}

// writeSectionLabel heads one column of the split card.
func writeSectionLabel(b *strings.Builder, y int, fill, text string) {
	fmt.Fprintf(b, `<g class="stagger" style="animation-delay: 300ms">`+
		`<text x="2" y="%d" class="section-label" fill="%s">%s</text>`+
		`<line x1="2" y1="%d" x2="%d" y2="%d" stroke="%s" stroke-opacity="0.35" stroke-width="1"/></g>`,
		y+12, fill, xmlEscape(text), y+19, splitColumnWidth-10, y+19, fill)
}

// writeLangRows draws one list of languages and returns the next delay index.
func writeLangRows(b *strings.Builder, langs []stats.LangStat, theme config.Theme, top, index int) int {
	for i, lang := range langs {
		writeLangRow(b, lang, theme, top+i*langRowHeight, index+i)
	}
	return index + len(langs)
}

// Langs renders the most-used-languages card.
func Langs(res stats.LangResult, cfg config.Languages) []byte {
	theme := cfg.Theme
	langs := res.Languages
	height := 45 + (len(langs)+1)*langRowHeight

	var b strings.Builder
	b.Grow(1024 + len(langs)*700)

	writeLangHead(&b, cfg, cardHead{width: langCardWidth, height: height, desc: langsDescription(langs)})

	b.WriteString(`<g data-testid="main-card-body" transform="translate(0, 55)">`)
	b.WriteString(`<svg data-testid="lang-items" x="25">`)
	writeLangRows(&b, langs, theme, 0, 0)
	b.WriteString(`</svg></g></svg>`)
	return []byte(b.String())
}

// cardHead is the per-layout part of a languages card header.
type cardHead struct {
	width, height int
	desc          string
	// centered puts the title over both columns of the split card; the
	// single-column card keeps the original left alignment.
	centered bool
}

// writeLangHead emits the opening tag, styles, background and title shared by
// both language layouts.
func writeLangHead(b *strings.Builder, cfg config.Languages, head cardHead) {
	theme := cfg.Theme
	width, height, desc := head.width, head.height, head.desc

	fmt.Fprintf(b, `<svg width="%d" height="%d" viewBox="0 0 %d %d" fill="none" `+
		`xmlns="http://www.w3.org/2000/svg" role="img" aria-labelledby="descId">`,
		width, height, width, height)

	fmt.Fprintf(b, `<title id="titleId">%s</title>`, xmlEscape(cfg.Title))
	fmt.Fprintf(b, `<desc id="descId">%s</desc>`, xmlEscape(desc))

	fmt.Fprintf(b, `<style>
.header {
  font: 600 18px 'Segoe UI', Ubuntu, Sans-Serif;
  fill: %s;
  animation: fadeInAnimation 0.8s ease-in-out forwards;
}
@supports(-moz-appearance: auto) { .header { font-size: 15.5px; } }
@keyframes slideInAnimation { from { width: 0; } to { width: calc(100%%-100px); } }
@keyframes growWidthAnimation { from { width: 0; } to { width: 100%%; } }
.lang-name { font: 400 11px "Segoe UI", Ubuntu, Sans-Serif; fill: %s; }
.section-label { font: 600 12px "Segoe UI", Ubuntu, Sans-Serif; letter-spacing: 0.5px; }
.stagger { opacity: 0; animation: fadeInAnimation 0.3s ease-in-out forwards; }
#rect-mask rect { animation: slideInAnimation 1s ease-in-out forwards; }
.lang-progress { animation: growWidthAnimation 0.6s ease-in-out forwards; }
@keyframes fadeInAnimation { from { opacity: 0; } to { opacity: 1; } }
</style>`, theme.Title, theme.Text)

	strokeOpacity := "1"
	if theme.HideBorder {
		strokeOpacity = "0"
	}
	fmt.Fprintf(b, `<rect data-testid="card-bg" x="0.5" y="0.5" rx="4.5" height="99%%" `+
		`stroke="%s" width="%d" fill="%s" stroke-opacity="%s"/>`,
		theme.Border, width-1, theme.Background, strokeOpacity)

	if head.centered {
		fmt.Fprintf(b, `<g data-testid="card-title" transform="translate(%d, 35)">`+
			`<g transform="translate(0, 0)">`+
			`<text x="0" y="0" class="header" data-testid="header" text-anchor="middle">%s</text></g></g>`,
			width/2, xmlEscape(cfg.Title))
		return
	}
	fmt.Fprintf(b, `<g data-testid="card-title" transform="translate(25, 35)">`+
		`<g transform="translate(0, 0)">`+
		`<text x="0" y="0" class="header" data-testid="header">%s</text></g></g>`,
		xmlEscape(cfg.Title))
}

// writeLangRow draws one language: name, percentage and progress bar.
func writeLangRow(b *strings.Builder, lang stats.LangStat, theme config.Theme, y, index int) {
	pct := formatPercent(lang.Percent)
	// A sliver of a bar is unreadable, so anything under 2% still draws at
	// 2%. The printed number stays exact.
	barPct := pct
	if lang.Percent < 2 {
		barPct = "2"
	}
	color := lang.Color
	if color == "" {
		color = theme.Text
	}

	fmt.Fprintf(b, `<g transform="translate(0, %d)">`, y)
	fmt.Fprintf(b, `<g class="stagger" style="animation-delay: %dms">`, 450+index*150)
	fmt.Fprintf(b, `<text data-testid="lang-name" x="2" y="15" class="lang-name">%s</text>`,
		xmlEscape(lang.Name))
	fmt.Fprintf(b, `<text x="215" y="34" class="lang-name">%s%%</text>`, pct)
	fmt.Fprintf(b, `<svg width="%d" x="0" y="25">`, langBarWidth)
	fmt.Fprintf(b, `<rect rx="5" ry="5" x="0" y="0" width="%d" height="8" fill="%s"></rect>`,
		langBarWidth, theme.Track)
	fmt.Fprintf(b, `<svg data-testid="lang-progress" width="%s%%">`, barPct)
	fmt.Fprintf(b, `<rect height="8" fill="%s" rx="5" ry="5" x="0" y="0" `+
		`class="lang-progress" style="animation-delay: %dms;"/>`, color, 750+index*150)
	b.WriteString(`</svg></svg></g></g>`)
}

func langsDescription(langs []stats.LangStat) string {
	if len(langs) == 0 {
		return "No languages to show"
	}
	parts := make([]string, 0, len(langs))
	for _, l := range langs {
		parts = append(parts, fmt.Sprintf("%s %s%%", l.Name, formatPercent(l.Percent)))
	}
	return strings.Join(parts, ", ")
}

// formatPercent rounds to two decimals and drops trailing zeros, so 60.40
// prints as 60.4 and 36.25 stays 36.25.
func formatPercent(v float64) string {
	s := strconv.FormatFloat(v, 'f', 2, 64)
	s = strings.TrimRight(s, "0")
	s = strings.TrimSuffix(s, ".")
	if s == "" || s == "-" {
		return "0"
	}
	return s
}

var xmlReplacer = strings.NewReplacer(
	"&", "&amp;",
	"<", "&lt;",
	">", "&gt;",
	`"`, "&quot;",
	"'", "&apos;",
)

func xmlEscape(s string) string { return xmlReplacer.Replace(s) }
