package store

// Cross-process proof that a read-only handle cannot corrupt live job state.
//
// The test re-executes the test binary as a second OS process, which is the
// situation the P0 bug describes: the CLI opens the store while the Agent
// process is running. Skip in -short mode or when the binary cannot be
// re-executed.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/shiuoei/print-bridge/internal/domain"
)

// TestHelperProcess is not a real test: it is the body of the second process
// started by TestOpenReadOnlySecondProcess.
func TestHelperProcess(t *testing.T) {
	dir := os.Getenv("PRINTBRIDGE_STORE_HELPER")
	if dir == "" {
		t.Skip("helper process only")
	}
	ctx := context.Background()
	ro, err := OpenReadOnly(dir)
	if err != nil {
		fmt.Printf("HELPER_OPEN_ERR=%v\n", err)
		os.Exit(1)
	}
	defer ro.Close()
	st, err := ro.Latest(ctx, "live")
	if err != nil {
		fmt.Printf("HELPER_LATEST_ERR=%v\n", err)
		os.Exit(1)
	}
	fmt.Printf("HELPER_STATUS=%s\n", st.Status)
	// Prove the child is genuinely read-only and cannot "help" by writing.
	if err := ro.AddStatus(ctx, domain.Status{JobID: "live", Status: "failed"}); err == nil {
		fmt.Println("HELPER_WRITE_LEAKED=1")
		os.Exit(1)
	} else if !errors.Is(err, ErrReadOnly) {
		fmt.Printf("HELPER_WRITE_ERR=%v\n", err)
		os.Exit(1)
	}
	fmt.Println("HELPER_WRITE_REJECTED=1")
}

func TestOpenReadOnlySecondProcess(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping subprocess test in -short mode")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Skipf("no test executable: %v", err)
	}

	dir := t.TempDir()
	w, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	ctx := context.Background()
	// The Agent is mid-flight: a queued job that must stay queued.
	if err := w.AddStatus(ctx, domain.Status{JobID: "live", Status: "queued", Message: "waiting"}); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(exe, "-test.run=TestHelperProcess", "-test.v=false")
	cmd.Env = append(os.Environ(), "PRINTBRIDGE_STORE_HELPER="+dir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helper process failed: %v\n%s", err, out)
	}
	if got, want := string(out), "HELPER_STATUS=queued"; !containsLine(got, want) {
		t.Fatalf("helper output missing %q:\n%s", want, got)
	}
	if want := "HELPER_WRITE_REJECTED=1"; !containsLine(string(out), want) {
		t.Fatalf("helper output missing %q:\n%s", want, out)
	}
	// The parent's (Agent's) view is untouched by the second process.
	st, err := w.Latest(ctx, "live")
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != "queued" {
		t.Fatalf("after second-process read-only open: status = %q, want queued", st.Status)
	}
	// And the writer still works.
	if err := w.AddStatus(ctx, domain.Status{JobID: "live", Status: "printing", Message: "spooling"}); err != nil {
		t.Fatalf("writer blocked after second process exited: %v", err)
	}

	// A second process on a cleanly closed database also cannot recover stale
	// nonterminal jobs, which is the exact P0 corruption mode.
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command(exe, "-test.run=TestHelperProcess", "-test.v=false")
	cmd.Env = append(os.Environ(), "PRINTBRIDGE_STORE_HELPER="+dir)
	out, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helper process on closed db failed: %v\n%s", err, out)
	}
	if !containsLine(string(out), "HELPER_STATUS=printing") {
		t.Fatalf("second process saw wrong state (recovery ran?):\n%s", out)
	}
	// Reopen writable: the job must still be printing, not failed.
	w2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer w2.Close()
	// Deliberately after Open: recovery is expected here, and proves the split
	// still recovers only on the exclusive path.
	got, err := w2.Latest(ctx, "live")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "failed" || got.Message != "interrupted by previous exit" {
		t.Fatalf("exclusive Open no longer recovers: %#v", got)
	}
}

func containsLine(haystack, needle string) bool {
	for _, line := range strings.Split(haystack, "\n") {
		if strings.TrimRight(line, "\r ") == needle {
			return true
		}
	}
	return false
}
