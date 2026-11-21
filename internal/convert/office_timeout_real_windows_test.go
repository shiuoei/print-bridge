//go:build windows

package convert

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The test starts a real WPS COM spreadsheet instance, pauses after ownership
// is proven, then shortens the same timeout path used in production. It does
// not submit anything to a printer or touch pre-existing WPS processes.
func TestRealOfficeTimeoutCleanup(t *testing.T) {
	if os.Getenv("PRINTBRIDGE_REAL_OFFICE_TIMEOUT") != "1" {
		t.Skip("set PRINTBRIDGE_REAL_OFFICE_TIMEOUT=1 to test WPS COM timeout cleanup")
	}
	root := os.Getenv("PRINTBRIDGE_MATRIX_OFFICE_DIR")
	if root == "" {
		t.Fatal("set PRINTBRIDGE_MATRIX_OFFICE_DIR to the synthetic Office corpus")
	}
	source := filepath.Join(root, "xlsx", "healthy-basic.xlsx")
	if _, err := os.Stat(source); err != nil {
		t.Fatal(err)
	}
	before, err := officeProcessSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	timeout := 5 * time.Second
	if os.Getenv("PRINTBRIDGE_REAL_OFFICE_FULL_TIMEOUT") == "1" {
		timeout = 120 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout+20*time.Second)
	defer cancel()
	owned := make(chan uint32, 1)
	hook := func(ctx context.Context, pid uint32) {
		owned <- pid
		<-ctx.Done()
	}
	path, err := windowsOfficePDFWithTimeoutAndHook(ctx, source, "xlsx", t.TempDir(), timeout, hook)
	if path != "" || err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("expected COM timeout, got path %q, err %v", path, err)
	}
	var pid uint32
	select {
	case pid = <-owned:
	default:
		t.Fatal("no new WPS COM process was identified; timeout cleanup was not exercised")
	}
	deadline := time.Now().Add(6 * time.Second)
	for {
		after, err := officeProcessSnapshot()
		if err != nil {
			t.Fatal(err)
		}
		if after[pid].name != "et.exe" {
			for oldPID, process := range before {
				if (process.name == "wpp.exe" || process.name == "wps.exe" || process.name == "et.exe") && after[oldPID].name != process.name {
					t.Fatalf("pre-existing user WPS process %d (%s) did not survive", oldPID, process.name)
				}
			}
			t.Logf("timeout cleaned new WPS COM process %d; pre-existing WPS processes survived", pid)
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("new WPS COM process %d remained after timeout", pid)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
