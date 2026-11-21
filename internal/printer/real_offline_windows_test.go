//go:build windows

package printer_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/shiuoei/print-bridge/internal/domain"
	"github.com/shiuoei/print-bridge/internal/printer"
)

type offlineJob struct {
	id     int
	name   string
	status string
}

// PDF uses the SumatraPDF path, which can behave differently from Winspool
// raw. It is separately gated so a repeat run cannot enqueue both formats.
func TestRealOfflinePDFSubmissionAndCleanup(t *testing.T) {
	name := os.Getenv("PRINTBRIDGE_REAL_OFFLINE_PRINTER")
	if name == "" || os.Getenv("PRINTBRIDGE_REAL_OFFLINE_PDF") != "1" {
		t.Skip("set PRINTBRIDGE_REAL_OFFLINE_PRINTER and PRINTBRIDGE_REAL_OFFLINE_PDF=1 while disconnected")
	}
	root := os.Getenv("PRINTBRIDGE_MATRIX_OFFICE_DIR")
	if root == "" {
		t.Fatal("set PRINTBRIDGE_MATRIX_OFFICE_DIR for the synthetic PDF fixture")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	initial, err := queryOfflineJobs(ctx)
	if err != nil || len(initial) != 0 {
		t.Fatalf("refusing to test a queue that cannot be read or has jobs: %+v, %v", initial, err)
	}
	fixture, err := os.ReadFile(filepath.Join(root, "pdf", "healthy-basic.pdf"))
	if err != nil {
		t.Fatal(err)
	}
	base := "printbridge-offline-pdf-test.pdf"
	path := filepath.Join(t.TempDir(), base)
	if err := os.WriteFile(path, fixture, 0600); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	_, printErr := printer.New().PrintPDF(ctx, path, name, 1, &domain.Paper{WidthMM: 100, HeightMM: 180})
	var observed offlineJob
	for time.Now().Before(started.Add(12 * time.Second)) {
		jobs, err := queryOfflineJobs(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(jobs) > 1 {
			t.Fatalf("unexpected concurrent jobs; manual cleanup required: %+v", jobs)
		}
		if len(jobs) == 1 {
			observed = jobs[0]
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if observed.id != 0 {
		if !strings.Contains(strings.ToLower(observed.name), "printbridge-offline-pdf-test") {
			t.Fatalf("refusing to remove unexpected job %+v", observed)
		}
		if err := removeOfflineJob(ctx, observed.id); err != nil {
			t.Fatal(err)
		}
		jobs, err := queryOfflineJobs(ctx)
		if err != nil || len(jobs) != 0 {
			t.Fatalf("PDF test job not fully removed: %+v, %v", jobs, err)
		}
	}
	t.Logf("offline PrintPDF returned %v after %s; queued job: %+v; cleanup verified", printErr, time.Since(started).Round(time.Millisecond), observed)
	if printErr == nil && observed.id == 0 {
		t.Fatal("PDF backend reported submitted but no offline queue job could be observed")
	}
}

func queryOfflineJobs(ctx context.Context) ([]offlineJob, error) {
	cmd := exec.CommandContext(ctx, "powershell", "-NoProfile", "-Command",
		`$ErrorActionPreference='Stop'; Get-PrintJob -PrinterName $env:PRINTBRIDGE_REAL_OFFLINE_PRINTER | ForEach-Object { "{0}`+"`t"+`{1}`+"`t"+`{2}" -f $_.ID,$_.DocumentName,$_.JobStatus }`)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("Get-PrintJob: %w: %s", err, strings.TrimSpace(string(out)))
	}
	var jobs []offlineJob
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.Split(strings.TrimSpace(line), "\t")
		if len(parts) != 3 {
			return nil, fmt.Errorf("unexpected print job row %q", line)
		}
		id, err := strconv.Atoi(parts[0])
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, offlineJob{id: id, name: parts[1], status: parts[2]})
	}
	return jobs, nil
}

func removeOfflineJob(ctx context.Context, id int) error {
	cmd := exec.CommandContext(ctx, "powershell", "-NoProfile", "-Command",
		`$ErrorActionPreference='Stop'; Remove-PrintJob -PrinterName $env:PRINTBRIDGE_REAL_OFFLINE_PRINTER -ID `+strconv.Itoa(id))
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("Remove-PrintJob %d: %w: %s", id, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Run only while the named USB printer is physically disconnected. The test
// removes its own queued job before the cable is reconnected.
func TestRealOfflineRawSubmissionAndCleanup(t *testing.T) {
	name := os.Getenv("PRINTBRIDGE_REAL_OFFLINE_PRINTER")
	if name == "" {
		t.Skip("set PRINTBRIDGE_REAL_OFFLINE_PRINTER only while that device is disconnected")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	initial, err := queryOfflineJobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(initial) != 0 {
		t.Fatalf("refusing to test a queue with existing jobs: %+v", initial)
	}
	// A recognizable one-label TSPL payload makes any unexpected output easy
	// to identify. No other queue is touched.
	payload := []byte("SIZE 100 mm,180 mm\r\nGAP 2 mm,0\r\nCLS\r\nTEXT 20,20,\"3\",0,1,1,\"PrintBridge offline test\"\r\nPRINT 1\r\n")
	started := time.Now()
	_, printErr := printer.New().PrintRaw(ctx, name, payload)
	var observed offlineJob
	for time.Now().Before(started.Add(12 * time.Second)) {
		jobs, err := queryOfflineJobs(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(jobs) > 1 {
			t.Fatalf("unexpected concurrent jobs; manual cleanup required: %+v", jobs)
		}
		if len(jobs) == 1 {
			observed = jobs[0]
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if observed.id != 0 {
		if observed.name != "PrintBridge Job" {
			t.Fatalf("refusing to remove unexpected job %+v", observed)
		}
		if err := removeOfflineJob(ctx, observed.id); err != nil {
			t.Fatal(err)
		}
		jobs, err := queryOfflineJobs(ctx)
		if err != nil || len(jobs) != 0 {
			t.Fatalf("test job not fully removed: %+v, %v", jobs, err)
		}
	}
	t.Logf("offline PrintRaw returned %v after %s; queued job: %+v; cleanup verified", printErr, time.Since(started).Round(time.Millisecond), observed)
	if printErr == nil && observed.id == 0 {
		t.Fatal("backend reported submitted but no offline queue job could be observed")
	}
}
