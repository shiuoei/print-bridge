//go:build windows

package convert

import (
	"testing"
	"time"
)

func TestTimedOutCOMCleansLateOwnedPID(t *testing.T) {
	for _, late := range []bool{false, true} {
		name := "already_known"
		if late {
			name = "arrives_after_timeout"
		}
		t.Run(name, func(t *testing.T) {
			pid := make(chan uint32, 1)
			result := make(chan comResult, 1)
			terminated := make(chan uint32, 1)
			if !late {
				pid <- 123
			}
			cleanupTimedOutCOMWith(pid, result, time.Now(), "KET.Application", func(p uint32, _ time.Time, progID string) {
				if progID != "KET.Application" {
					t.Errorf("unexpected progID %q", progID)
				}
				terminated <- p
			})
			if late {
				pid <- 123
			}
			select {
			case p := <-terminated:
				if p != 123 {
					t.Fatalf("terminated PID %d", p)
				}
			case <-time.After(time.Second):
				t.Fatal("newly owned process was not cleaned after timeout")
			}
		})
	}
}

func TestFindNewCOMProcessNeverSelectsExistingUserSession(t *testing.T) {
	before := map[uint32]officeProcess{
		1: {name: "svchost.exe"},
		2: {name: "explorer.exe"},
		3: {name: "wps.exe", parent: 2},
		4: {name: "wps.exe", parent: 1},
	}
	after := map[uint32]officeProcess{
		1: {name: "svchost.exe"},
		2: {name: "explorer.exe"},
		3: {name: "wps.exe", parent: 2},
		4: {name: "wps.exe", parent: 1},
		5: {name: "wps.exe", parent: 1},
		6: {name: "wps.exe", parent: 5},
	}
	created := func(uint32) bool { return true }
	if got := findNewCOMProcess(before, after, "KWPS.Application", created); got != 5 {
		t.Fatalf("owned PID = %d, want new COM broker 5", got)
	}
	delete(after, 5)
	if got := findNewCOMProcess(before, after, "KWPS.Application", created); got != 0 {
		t.Fatalf("selected user or child process %d", got)
	}
	if got := findNewCOMProcess(before, before, "KWPS.Application", created); got != 0 {
		t.Fatalf("selected existing process %d", got)
	}
}

func TestCOMServiceAncestorExcludesUserLaunch(t *testing.T) {
	processes := map[uint32]officeProcess{
		1: {name: "svchost.exe"}, 2: {name: "explorer.exe"},
		3: {name: "wps.exe", parent: 2},
		4: {name: "wps.exe", parent: 1},
		5: {name: "wps.exe", parent: 4},
	}
	if hasCOMServiceAncestor(processes, 3) || !hasCOMServiceAncestor(processes, 5) {
		t.Fatal("failed to distinguish a user launch from a COM process tree")
	}
}

func TestFindNewCOMProcessRefusesAmbiguousNewInstances(t *testing.T) {
	before := map[uint32]officeProcess{1: {name: "svchost.exe"}}
	after := map[uint32]officeProcess{
		1: {name: "svchost.exe"},
		5: {name: "wps.exe", parent: 1},
		6: {name: "wps.exe", parent: 1},
	}
	if got := findNewCOMProcess(before, after, "KWPS.Application", func(uint32) bool { return true }); got != 0 {
		t.Fatalf("selected ambiguous process %d", got)
	}
}
