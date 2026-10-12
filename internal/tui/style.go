package tui

import (
	"fmt"
	"math"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/lucasb-eyer/go-colorful"
)

// The palette is deliberately minimal: the terminal's default foreground for
// content, gray for secondary text and borders, and color only where it
// carries meaning (red for uncommitted changes and errors, yellow for
// warnings). Colors are ANSI indices so they follow the terminal's theme.
var (
	colMuted = lipgloss.Color("8")
	colWarn  = lipgloss.Color("3")
	colErr   = lipgloss.Color("1")
)

var (
	plain   = lipgloss.NewStyle()
	stBold  = lipgloss.NewStyle().Bold(true)
	stMuted = lipgloss.NewStyle().Foreground(colMuted)
	stWarn  = lipgloss.NewStyle().Foreground(colWarn)
	stErr   = lipgloss.NewStyle().Foreground(colErr)
	stKey   = plain
)

// The selected entry is the one flourish in an otherwise monochrome
// interface: a gradient that drifts right on a loop. The colors form a
// periodic wave over a narrow band of hues, so the loop has no seam. Hues come
// from OKLCH at constant lightness and chroma, which keeps every step equally
// bright; lightness and chroma stay low so the selection reads as a tint
// rather than neon.
const (
	gradientPeriod = 28.0 // cells per full wave
	gradientSpeed  = 10.0 // cells per second the wave travels right
	gradientSteps  = 96   // precomputed colors per wave
)

// palette is one band of hues for the selection gradient.
type palette struct {
	hue, swing        float64 // hue varies ±swing degrees around hue
	lightness, chroma float64
	still             bool // no color and no motion
}

var palettes = map[string]palette{
	"tide":   {hue: 215, swing: 32, lightness: 0.74, chroma: 0.085}, // teal to blue
	"ember":  {hue: 50, swing: 28, lightness: 0.74, chroma: 0.095},  // coral to amber
	"orchid": {hue: 302, swing: 52, lightness: 0.70, chroma: 0.10},  // rose to violet
	"mono":   {still: true},
}

var (
	gradientStyles []lipgloss.Style // one bold style per step of the wave
	gradientStill  bool
	// gradientShift is how far (in cells) the wave has traveled. The app
	// advances it on each animation frame; views read it while rendering,
	// on the same goroutine.
	gradientShift float64
)

func init() { setPalette("tide") }

// setPalette switches the selection gradient, falling back to tide.
func setPalette(name string) {
	p, ok := palettes[name]
	if !ok {
		p = palettes["tide"]
	}
	gradientStill = p.still
	gradientStyles = make([]lipgloss.Style, gradientSteps)
	for i := range gradientStyles {
		if p.still {
			gradientStyles[i] = stBold
			continue
		}
		u := float64(i) / gradientSteps
		h := p.hue + p.swing*math.Cos(2*math.Pi*u)
		c := colorful.OkLch(p.lightness, p.chroma, math.Mod(h+360, 360)).Clamped()
		gradientStyles[i] = lipgloss.NewStyle().Bold(true).Foreground(c)
	}
}

func gradientAt(x int) lipgloss.Style {
	u := (float64(x) - gradientShift) / gradientPeriod
	u -= math.Floor(u)
	return gradientStyles[int(u*gradientSteps)%gradientSteps]
}

// rainbow renders text with the moving gradient.
func rainbow(text string) string {
	if gradientStill {
		return stBold.Render(text)
	}
	var b strings.Builder
	for i, r := range []rune(text) {
		if r == ' ' {
			b.WriteRune(r)
			continue
		}
		b.WriteString(gradientAt(i + 2).Render(string(r)))
	}
	return b.String()
}

// selMarker points at the selected row in the gradient's current color.
func selMarker() string { return gradientAt(0).Render("›") }

// width is the display width of s, ignoring escape sequences.
func width(s string) int { return ansi.StringWidth(s) }

// truncate cuts s to w cells, ending with an ellipsis when it had to cut.
func truncate(s string, w int) string {
	if w <= 0 {
		return ""
	}
	return ansi.Truncate(s, w, "…")
}

// pad truncates or right-pads s to exactly w cells.
func pad(s string, w int) string {
	s = truncate(s, w)
	if n := w - width(s); n > 0 {
		s += strings.Repeat(" ", n)
	}
	return s
}

// spread places left and right on one line of width w, truncating left first.
func spread(left, right string, w int) string {
	rw := width(right)
	if rw >= w {
		return truncate(right, w)
	}
	return pad(left, w-rw-1) + " " + right
}

// panel lays out a screen section of exactly w×h cells without a border,
// inside a two-cell gutter: a title and a blank row (omitted when title is
// empty), the body, and a blank last row. Body lines past the space are
// clipped.
func panel(title string, body []string, w, h int, focused bool) string {
	if w < 4 || h < 2 {
		return ""
	}
	inner := w - 4
	ts := stMuted
	if focused {
		ts = stBold
	}
	lines := make([]string, 0, h)
	rows := h - 1
	if title != "" {
		lines = append(lines, "  "+pad(ts.Render(truncate(title, inner)), inner)+"  ", strings.Repeat(" ", w))
		rows -= 2
	}
	for i := 0; i < rows; i++ {
		line := ""
		if i < len(body) {
			line = body[i]
		}
		lines = append(lines, "  "+pad(line, inner)+"  ")
	}
	lines = append(lines, strings.Repeat(" ", w))
	return strings.Join(lines, "\n")
}

// box draws a rounded box of exactly w×h cells with title set into the top
// border, like `╭─ Title ────╮`. Only dialogs use it.
func box(title string, body []string, w, h int, focused bool) string {
	if w < 4 || h < 2 {
		return ""
	}
	bc := stMuted
	ts := stMuted // focus shows in the title, not a colored border
	if focused {
		ts = stBold
	}
	inner := w - 2
	var top string
	if title != "" {
		t := " " + truncate(title, inner-4) + " "
		top = bc.Render("╭─") + ts.Render(t) + bc.Render(strings.Repeat("─", max(inner-1-width(t), 0))+"╮")
	} else {
		top = bc.Render("╭" + strings.Repeat("─", inner) + "╮")
	}
	lines := make([]string, 0, h)
	lines = append(lines, top)
	side := bc.Render("│")
	for i := 0; i < h-2; i++ {
		line := ""
		if i < len(body) {
			line = body[i]
		}
		lines = append(lines, side+" "+pad(line, inner-2)+" "+side)
	}
	lines = append(lines, bc.Render("╰"+strings.Repeat("─", inner)+"╯"))
	return strings.Join(lines, "\n")
}

// Forms share one layout: a lowercase label column, then the value. Notes
// under a form start at the value column.
const labelW = 10 // same column as branch names on the home screen

var formIndent = strings.Repeat(" ", labelW)

// field renders one form row; the focused field's label is not dimmed.
func field(label string, focused bool, value string) string {
	st := stMuted
	if focused {
		st = plain
	}
	return st.Render(pad(label, labelW)) + value
}

// note renders text under a form, wrapped and aligned to the value column.
func note(text string, w int, st lipgloss.Style) []string {
	var out []string
	for _, l := range wrap(text, w-labelW) {
		out = append(out, formIndent+st.Render(l))
	}
	return out
}

// rule is a horizontal separator for inside panels.
func rule(w int) string { return stMuted.Render(strings.Repeat("─", max(w, 0))) }

// hint is one key/description pair in the footer.
type hint struct{ key, desc string }

func renderHints(hs []hint, w int) string {
	line := ""
	for i, h := range hs {
		part := stKey.Render(h.key) + " " + stMuted.Render(h.desc)
		next := line
		if i > 0 {
			next += "   "
		}
		next += part
		if width(next)+4 > w {
			break
		}
		line = next
	}
	return "  " + line
}

// clock formats t for a detail line: 15:04 today, Mon 15:04 this week,
// Jan 2 otherwise.
func clock(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	now := time.Now()
	switch {
	case t.YearDay() == now.YearDay() && t.Year() == now.Year():
		return t.Format("15:04")
	case now.Sub(t) < 6*24*time.Hour:
		return t.Format("Mon 15:04")
	case t.Year() == now.Year():
		return t.Format("Jan 2")
	default:
		return t.Format("Jan 2 2006")
	}
}

// ago formats the time since t compactly: 45s, 12m, 3h, 5d, 3w, 4mo, 2y.
func ago(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d < 14*24*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	case d < 60*24*time.Hour:
		return fmt.Sprintf("%dw", int(d.Hours()/24/7))
	case d < 365*24*time.Hour:
		return fmt.Sprintf("%dmo", int(d.Hours()/24/30))
	default:
		return fmt.Sprintf("%dy", int(d.Hours()/24/365))
	}
}

// duration formats an elapsed time for the run screen: 850ms, 12.3s, 4m05s.
func duration(d time.Duration) string {
	switch {
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	default:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
}

// plural returns "1 file" / "3 files".
func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

// tildify abbreviates the home directory in a path.
func tildify(p, home string) string {
	if home != "" && (p == home || strings.HasPrefix(p, home+"/")) {
		return "~" + p[len(home):]
	}
	return p
}

// wrap breaks s into lines of at most w cells.
func wrap(s string, w int) []string {
	if w <= 0 {
		return nil
	}
	return strings.Split(ansi.Wordwrap(s, w, ""), "\n")
}

// center places block (multi-line) in the middle of a w×h area.
func center(block string, w, h int) string {
	return lipgloss.Place(w, h, lipgloss.Center, lipgloss.Center, block)
}
