package printer

import (
	"strconv"
	"strings"
)

// This file holds the CUPS output parsing helpers. They are pure string logic and
// deliberately carry no build tag, so the behaviour that decides whether a job can
// be reported `completed` is testable on every platform, not only on a host with a
// working CUPS installation.

// parseCUPSJobID extracts the queue job id from `lp` output such as
// "request id is HP_LaserJet-42 (1 file(s))".
//
// The id is the only handle CUPS gives this agent for tracking, so a parse miss
// silently downgrades every job to `submitted`.
func parseCUPSJobID(out string) string {
	const marker = "request id is "
	idx := strings.Index(out, marker)
	if idx < 0 {
		return ""
	}
	rest := strings.TrimSpace(out[idx+len(marker):])
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return ""
	}
	return strings.TrimSuffix(fields[0], ".")
}

// cupsCompletedContains reports whether a `lpstat -W completed -o` listing holds
// the given job id.
//
// Only the first whitespace-separated token of each line is considered, and it
// must match exactly: a substring test would make Printer-1 match Printer-10 and
// report the wrong job as completed.
func cupsCompletedContains(listing, jobID string) bool {
	for _, line := range strings.Split(listing, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if strings.TrimSuffix(fields[0], ".") == jobID {
			return true
		}
	}
	return false
}

// mediaTokenFromName turns a driver paper display name into a media token. PPD
// keywords contain no spaces; a display name does, so spaces become underscores.
func mediaTokenFromName(name string) string {
	return strings.ReplaceAll(strings.TrimSpace(name), " ", "_")
}

// formatMM renders a millimetre value without trailing zeros, which is the form
// CUPS accepts and `lpoptions` prints.
func formatMM(v float64) string {
	s := strconv.FormatFloat(v, 'f', 2, 64)
	s = strings.TrimSuffix(s, "0")
	s = strings.TrimSuffix(s, "0")
	s = strings.TrimSuffix(s, ".")
	return s
}
