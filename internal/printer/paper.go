package printer

import (
	"strings"

	"github.com/shiuoei/print-bridge/internal/domain"
)

// paperMatchToleranceMM is how far a requested size may differ from a driver
// paper and still count as the same stock. Drivers advertise sizes in tenths of a
// millimetre while callers describe labels in round numbers, so an exact float
// comparison would reject real matches.
const paperMatchToleranceMM = 1.0

// resolvePaperName maps a requested width/height in millimetres onto the exact
// paper name the printer driver advertises.
//
// This exists because SumatraPDF's `-print-settings paper=` accepts a *driver
// paper name*, not a dimension expression. Measured on an HPRT N41 label printer:
//
//	paper=100x180mm              -> DEVMODE 76x130 mm (driver default; ignored)
//	paper=Custom.100x180mm       -> DEVMODE 76x130 mm (ignored)
//	paper=A4                     -> DEVMODE 76x130 mm (ignored)
//	paper=菜鸟标准 100mm * 180mm -> DEVMODE 100x180 mm (applied)
//
// So the requested size must be translated into the driver's own vocabulary,
// otherwise the job silently prints on the device default.
//
// Orientation is not significant: a 180x100 request matches a 100x180 driver
// paper. Names containing a comma or newline are skipped because SumatraPDF
// splits -print-settings on commas, so such a name cannot be expressed there.
//
// It returns the matched name and true, or "" and false when no advertised paper
// is within tolerance.
func resolvePaperName(papers []domain.PrinterPaper, want *domain.Paper) (string, bool) {
	if want == nil || len(papers) == 0 {
		return "", false
	}
	bestName := ""
	bestErr := 0.0
	for _, paper := range papers {
		if paper.Name == "" || strings.ContainsAny(paper.Name, ",\r\n") {
			continue
		}
		err, ok := paperDistance(paper, want)
		if !ok {
			continue
		}
		if bestName == "" || err < bestErr {
			bestName, bestErr = paper.Name, err
		}
	}
	if bestName == "" {
		return "", false
	}
	return bestName, true
}

// paperDistance reports the total millimetre error when the driver paper matches
// the requested stock in either orientation.
func paperDistance(paper domain.PrinterPaper, want *domain.Paper) (float64, bool) {
	directW := absDiff(paper.WidthMM, want.WidthMM)
	directH := absDiff(paper.HeightMM, want.HeightMM)
	if directW <= paperMatchToleranceMM && directH <= paperMatchToleranceMM {
		return directW + directH, true
	}
	swappedW := absDiff(paper.WidthMM, want.HeightMM)
	swappedH := absDiff(paper.HeightMM, want.WidthMM)
	if swappedW <= paperMatchToleranceMM && swappedH <= paperMatchToleranceMM {
		return swappedW + swappedH, true
	}
	return 0, false
}

func absDiff(a, b float64) float64 {
	if a > b {
		return a - b
	}
	return b - a
}
