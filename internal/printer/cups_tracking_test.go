package printer

import "testing"

// TestParseCUPSJobID covers the real `lp` output shapes. The job id is the only
// handle CUPS gives us for tracking, so a parse miss silently downgrades every
// job to `submitted`.
func TestParseCUPSJobID(t *testing.T) {
	cases := []struct {
		out  string
		want string
	}{
		{"request id is HP_LaserJet-42 (1 file(s))\n", "HP_LaserJet-42"},
		{"request id is label-printer-123.\n", "label-printer-123"},
		{"request id is Printer-7 (2 file(s))\r\n", "Printer-7"},
		{"", ""},
		{"lp: Error - no default destination available.\n", ""},
	}
	for _, c := range cases {
		if got := parseCUPSJobID(c.out); got != c.want {
			t.Errorf("parseCUPSJobID(%q) = %q, want %q", c.out, got, c.want)
		}
	}
}

// TestCupsCompletedContainsUsesExactTokens is the regression guard for a
// substring match: Printer-1 must not match Printer-10, or the agent would report
// the wrong job as completed.
func TestCupsCompletedContainsUsesExactTokens(t *testing.T) {
	listing := "Printer-1  user  1024  Mon 01 Jan 2026 10:00:00 AM\n" +
		"Printer-10  user  2048  Mon 01 Jan 2026 10:05:00 AM\n" +
		"other-2.  user  512  Mon 01 Jan 2026 10:10:00 AM\n"

	if !cupsCompletedContains(listing, "Printer-1") {
		t.Error("Printer-1 should match")
	}
	if !cupsCompletedContains(listing, "Printer-10") {
		t.Error("Printer-10 should match")
	}
	if !cupsCompletedContains(listing, "other-2") {
		t.Error("a trailing dot should be ignored")
	}
	for _, absent := range []string{"Printer-1x", "rinter-1", "Printer-100"} {
		if cupsCompletedContains(listing, absent) {
			t.Errorf("%q must not match any line", absent)
		}
	}
	if cupsCompletedContains("", "Printer-1") {
		t.Error("an empty listing must not match")
	}
}

// TestFormatMM keeps the media token free of trailing zeros, which is the form
// CUPS and lpoptions use.
func TestFormatMM(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{100, "100"},
		{100.5, "100.5"},
		{76.2, "76.2"},
		{0.5, "0.5"},
		{210, "210"},
	}
	for _, c := range cases {
		if got := formatMM(c.in); got != c.want {
			t.Errorf("formatMM(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestMediaTokenFromName turns a display name into a PPD-style token.
func TestMediaTokenFromName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"A4", "A4"},
		{"菜鸟标准 100mm * 180mm", "菜鸟标准_100mm_*_180mm"},
		{"  Letter  ", "Letter"},
	}
	for _, c := range cases {
		if got := mediaTokenFromName(c.in); got != c.want {
			t.Errorf("mediaTokenFromName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
