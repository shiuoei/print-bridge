package main

// Exit codes are a stable, documented contract (docs/FEATURES.md C03). Scripts
// branch on these numbers, so they must not be renumbered casually.
const (
	exitOK          = 0
	exitDoctorFail  = 1
	exitUsage       = 2
	exitAgentAbsent = 3
	exitPermission  = 4
	exitConflict    = 5
)
