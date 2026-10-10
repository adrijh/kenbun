package tui

import (
	"math"
	"strconv"
	"testing"
)

// Text must stay readable everywhere: WCAG AA (4.5:1) at least, AAA (7:1) for
// body text, the selected frame and highlights; the bold seal logo is large
// text (3:1).
func TestCellContrast(t *testing.T) {
	check := func(fg, bg string, want float64) {
		t.Helper()
		if r := contrast(fg, bg); r < want {
			t.Errorf("%s on %s: contrast %.1f, want >= %.1f", fg, bg, r, want)
		}
	}
	for _, bg := range warmHex {
		check(warmFg, bg, 4.5)
	}
	for _, bg := range coolHex {
		check(coolFg, bg, 4.5)
	}
	check(inkHex, greenHex, 7)
	check(inkHex, carpHex, 7)
	check(inkHex, fujiHex, 7)
	check(fujiHex, bgHex, 7)
	check(violetHex, bgHex, 4.5)
	check(fujiHex, selRowHex, 4.5)
	check(fujiHex, sealHex, 3)
}

func contrast(a, b string) float64 {
	la, lb := luminance(a), luminance(b)
	return (max(la, lb) + 0.05) / (min(la, lb) + 0.05)
}

func luminance(hex string) float64 {
	var l float64
	for i, w := range []float64{0.2126, 0.7152, 0.0722} {
		v, _ := strconv.ParseUint(hex[1+2*i:3+2*i], 16, 8)
		c := float64(v) / 255
		if c <= 0.03928 {
			c /= 12.92
		} else {
			c = math.Pow((c+0.055)/1.055, 2.4)
		}
		l += w * c
	}
	return l
}
