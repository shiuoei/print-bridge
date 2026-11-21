//go:build windows

package convert

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unsafe"

	"github.com/go-ole/go-ole"
	"github.com/go-ole/go-ole/oleutil"
	"golang.org/x/sys/windows"
)

type comResult struct {
	path string
	err  error
}

func windowsOfficePDF(ctx context.Context, input, format, dir string) (string, error) {
	return windowsOfficePDFWithTimeout(ctx, input, format, dir, 120*time.Second)
}

func windowsOfficePDFWithTimeout(ctx context.Context, input, format, dir string, timeout time.Duration) (string, error) {
	return windowsOfficePDFWithTimeoutAndHook(ctx, input, format, dir, timeout, nil)
}

// afterOwned is an internal test seam: it runs only after a newly created COM
// process has been identified. Production always passes nil.
func windowsOfficePDFWithTimeoutAndHook(ctx context.Context, input, format, dir string, timeout time.Duration, afterOwned func(context.Context, uint32)) (string, error) {
	candidates := map[string][]string{
		"docx": {"Word.Application", "KWPS.Application"},
		"xlsx": {"Excel.Application", "KET.Application"},
		"pptx": {"PowerPoint.Application", "KWPP.Application"},
	}[format]
	for _, progID := range candidates {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		result := make(chan comResult, 1)
		started := time.Now()
		pid := make(chan uint32, 1)
		workCtx, cancel := context.WithCancel(ctx)
		go func() {
			path, err := convertCOM(workCtx, input, format, dir, progID, started, pid, afterOwned)
			result <- comResult{path, err}
		}()
		timer := time.NewTimer(timeout)
		select {
		case r := <-result:
			timer.Stop()
			cancel()
			if r.err == nil {
				return r.path, nil
			}
			if r.err != ErrOfficeUnavailable {
				return "", r.err
			}
		case <-ctx.Done():
			timer.Stop()
			cancel()
			cleanupTimedOutCOM(pid, result, started, progID)
			return "", ctx.Err()
		case <-timer.C:
			cancel()
			cleanupTimedOutCOM(pid, result, started, progID)
			return "", fmt.Errorf("Office conversion timeout")
		}
	}
	return "", ErrOfficeUnavailable
}

// A timeout can occur before COM publishes its newly created PID. Keep a
// cleanup watcher for that late PID instead of abandoning the worker.
func cleanupTimedOutCOM(pid <-chan uint32, result <-chan comResult, started time.Time, progID string) {
	cleanupTimedOutCOMWith(pid, result, started, progID, terminateOwned)
}

func cleanupTimedOutCOMWith(pid <-chan uint32, result <-chan comResult, started time.Time, progID string, terminate func(uint32, time.Time, string)) {
	select {
	case p := <-pid:
		terminate(p, started, progID)
	default:
		go func() {
			select {
			case p := <-pid:
				terminate(p, started, progID)
			case <-result:
				// convertCOM has already run its deferred document close and app Quit.
			}
		}()
	}
}

func convertCOM(ctx context.Context, input, format, dir, progID string, started time.Time, pidCh chan<- uint32, afterOwned func(context.Context, uint32)) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := ole.CoInitializeEx(0, ole.COINIT_APARTMENTTHREADED); err != nil {
		return "", ErrOfficeUnavailable
	}
	defer ole.CoUninitialize()
	before, _ := officeProcessSnapshot()
	unknown, err := oleutil.CreateObject(progID)
	if err != nil {
		return "", ErrOfficeUnavailable
	}
	defer unknown.Release()
	app, err := unknown.QueryInterface(ole.IID_IDispatch)
	if err != nil {
		return "", ErrOfficeUnavailable
	}
	defer app.Release()
	ownedPID, err := appOwnedPID(app, started, before)
	if err != nil {
		after, _ := officeProcessSnapshot()
		ownedPID = findNewCOMProcess(before, after, progID, func(pid uint32) bool {
			return processCreatedAfter(pid, started.Add(-5*time.Second))
		})
		if ownedPID == 0 {
			return "", ErrOfficeUnavailable
		}
	}
	select {
	case pidCh <- ownedPID:
	default:
	}
	defer oleutil.CallMethod(app, "Quit")
	if afterOwned != nil {
		afterOwned(ctx, ownedPID)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	_, _ = oleutil.PutProperty(app, "Visible", false)
	_, _ = oleutil.PutProperty(app, "DisplayAlerts", 0)
	// Macro automation must be forced off for every document this agent opens.
	// Treating a failed set as best-effort would let a document that carries a
	// macro run under the user's own Office security settings, which the product
	// promises never to happen, so the conversion is refused instead.
	if _, err := oleutil.PutProperty(app, "AutomationSecurity", 3); err != nil {
		return "", fmt.Errorf("%w: the converter would not disable macro automation: %v", ErrOfficeUnavailable, err)
	}
	// External links make Office prompt on the automation thread, which blocks
	// until the conversion timeout expires. Both properties are format-specific:
	// Word has UpdateLinksAtOpen, Excel has AskToUpdateLinks.
	switch format {
	case "docx":
		_, _ = oleutil.PutProperty(app, "UpdateLinksAtOpen", false)
	case "xlsx":
		_, _ = oleutil.PutProperty(app, "AskToUpdateLinks", false)
	}
	out := filepath.Join(dir, "document.pdf")
	var collection, doc *ole.IDispatch
	var v *ole.VARIANT
	if format == "docx" {
		v, err = oleutil.GetProperty(app, "Documents")
	}
	if format == "xlsx" {
		v, err = oleutil.GetProperty(app, "Workbooks")
	}
	if format == "pptx" {
		v, err = oleutil.GetProperty(app, "Presentations")
	}
	if err != nil {
		return "", fmt.Errorf("Office collection: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	collection = v.ToIDispatch()
	defer v.Clear()
	if format == "xlsx" {
		v, err = oleutil.CallMethod(collection, "Open", input, 0, true)
	} else if format == "pptx" {
		v, err = oleutil.CallMethod(collection, "Open", input, -1, 0, 0)
	} else {
		v, err = oleutil.CallMethod(collection, "Open", input, false, true, false)
	}
	if err != nil {
		return "", fmt.Errorf("Office open: %w", err)
	}
	doc = v.ToIDispatch()
	defer v.Clear()
	defer oleutil.CallMethod(doc, "Close", false)
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if format == "pptx" {
		_, err = oleutil.CallMethod(doc, "SaveAs", out, 32)
	} else if format == "xlsx" {
		_, err = oleutil.CallMethod(doc, "ExportAsFixedFormat", 0, out)
	} else {
		_, err = oleutil.CallMethod(doc, "ExportAsFixedFormat", out, 17)
	}
	if err != nil {
		return "", fmt.Errorf("Office export PDF: %w", err)
	}
	if err := VerifyPDF(out); err != nil {
		return "", err
	}
	return out, nil
}

func appOwnedPID(app *ole.IDispatch, started time.Time, before map[uint32]officeProcess) (uint32, error) {
	if before == nil {
		return 0, ErrOfficeUnavailable
	}
	v, err := oleutil.GetProperty(app, "Hwnd")
	if err != nil {
		v, err = oleutil.GetProperty(app, "HWND")
	}
	if err != nil {
		return 0, err
	}
	hwnd := windows.HWND(v.Val)
	v.Clear()
	var pid uint32
	_, err = windows.GetWindowThreadProcessId(hwnd, &pid)
	if err != nil || pid == 0 {
		return 0, ErrOfficeUnavailable
	}
	after, err := officeProcessSnapshot()
	if err != nil {
		return 0, ErrOfficeUnavailable
	}
	if _, existed := before[pid]; existed || !hasCOMServiceAncestor(after, pid) || !processCreatedAfter(pid, started.Add(-5*time.Second)) {
		return 0, ErrOfficeUnavailable
	}
	return pid, nil
}

type officeProcess struct {
	name   string
	parent uint32
}

func hasCOMServiceAncestor(processes map[uint32]officeProcess, pid uint32) bool {
	for range 4 {
		process, ok := processes[pid]
		if !ok {
			return false
		}
		if process.name == "svchost.exe" {
			return true
		}
		pid = process.parent
	}
	return false
}

func officeProcessSnapshot() (map[uint32]officeProcess, error) {
	h, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(h)
	var entry windows.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	if err := windows.Process32First(h, &entry); err != nil {
		return nil, err
	}
	processes := make(map[uint32]officeProcess)
	for {
		processes[entry.ProcessID] = officeProcess{
			name: strings.ToLower(windows.UTF16ToString(entry.ExeFile[:])), parent: entry.ParentProcessID,
		}
		if err := windows.Process32Next(h, &entry); err != nil {
			break
		}
	}
	return processes, nil
}

// Some WPS COM objects do not expose Hwnd. Only a newly created Office
// process launched by the COM service host can be considered ours; an existing
// user session, or an ambiguous pair of new instances, must not be touched.
func findNewCOMProcess(before, after map[uint32]officeProcess, progID string, created func(uint32) bool) uint32 {
	executable := officeExecutable(progID)
	if executable == "" || before == nil || after == nil {
		return 0
	}
	var found uint32
	for pid, process := range after {
		if _, existed := before[pid]; existed || process.name != executable || after[process.parent].name != "svchost.exe" || !created(pid) {
			continue
		}
		if found != 0 {
			return 0
		}
		found = pid
	}
	return found
}

func officeExecutable(progID string) string {
	return map[string]string{
		"Word.Application": "winword.exe", "Excel.Application": "excel.exe", "PowerPoint.Application": "powerpnt.exe",
		"KWPS.Application": "wps.exe", "KET.Application": "et.exe", "KWPP.Application": "wpp.exe",
	}[progID]
}

func processCreatedAfter(pid uint32, threshold time.Time) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var created, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &created, &exit, &kernel, &user); err != nil {
		return false
	}
	return time.Unix(0, created.Nanoseconds()).After(threshold)
}
func terminateOwned(pid uint32, started time.Time, progID string) {
	if !processCreatedAfter(pid, started.Add(-5*time.Second)) {
		return
	}
	processes, err := officeProcessSnapshot()
	if err != nil || processes[pid].name != officeExecutable(progID) || !hasCOMServiceAncestor(processes, pid) {
		return
	}
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, pid)
	if err != nil {
		return
	}
	defer windows.CloseHandle(h)
	_ = windows.TerminateProcess(h, 1)
}
