package render

import "testing"

// TestFormatPercent pins the number formatting the card uses: two decimals,
// with trailing zeros dropped, so 60.40 prints as 60.4 and 36.25 stays 36.25.
func TestFormatPercent(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{60.4000001, "60.4"},
		{36.25, "36.25"},
		{24.6149, "24.61"},
		{0.12, "0.12"},
		{0.804, "0.8"},
		{2, "2"},
		{0, "0"},
		{100, "100"},
		{0.001, "0"},
	}
	for _, c := range cases {
		if got := formatPercent(c.in); got != c.want {
			t.Errorf("formatPercent(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestFormatThousands(t *testing.T) {
	cases := []struct {
		in   int
		want string
	}{
		{0, "0"},
		{7, "7"},
		{999, "999"},
		{1000, "1,000"},
		{9878, "9,878"},
		{9882, "9,882"},
		{1234567, "1,234,567"},
		{-9878, "-9,878"},
	}
	for _, c := range cases {
		if got := formatThousands(c.in); got != c.want {
			t.Errorf("formatThousands(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestXMLEscape matters because language and repository names reach the SVG.
func TestXMLEscape(t *testing.T) {
	got := xmlEscape(`C++ & "R" <tag>`)
	want := `C++ &amp; &quot;R&quot; &lt;tag&gt;`
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
