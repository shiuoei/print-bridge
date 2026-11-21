//go:build windows

package printer

import (
	"testing"

	"github.com/shiuoei/print-bridge/internal/domain"
)

// TestPaperOrientation matches the documented rule: a wider-than-tall request is
// landscape, and square stock stays portrait so it is not rotated needlessly.
func TestPaperOrientation(t *testing.T) {
	cases := []struct {
		paper *domain.Paper
		want  string
	}{
		{&domain.Paper{WidthMM: 100, HeightMM: 180}, "portrait"},
		{&domain.Paper{WidthMM: 180, HeightMM: 100}, "landscape"},
		{&domain.Paper{WidthMM: 50, HeightMM: 50}, "portrait"},
		{nil, "portrait"},
	}
	for _, c := range cases {
		if got := paperOrientation(c.paper); got != c.want {
			t.Errorf("paperOrientation(%v) = %q, want %q", c.paper, got, c.want)
		}
	}
}
