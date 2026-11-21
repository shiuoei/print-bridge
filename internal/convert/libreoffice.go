package convert

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// findLibreOffice locates a LibreOffice executable.
//
// A plain exec.LookPath is not enough: the official Windows and macOS installers
// do not put soffice on PATH, so a machine with LibreOffice installed would be
// reported as "no converter available" and every DOCX/PPTX job failed even
// though the shared office suites only cover some formats. The well-known
// install locations are therefore checked explicitly.
func findLibreOffice() (string, error) {
	for _, name := range []string{"libreoffice", "soffice"} {
		if p, err := exec.LookPath(name); err == nil {
			return p, nil
		}
	}
	for _, candidate := range libreOfficeCandidates() {
		if fi, err := os.Stat(candidate); err == nil && !fi.IsDir() {
			return candidate, nil
		}
	}
	return "", errors.New("LibreOffice executable not found")
}

// libreOfficeCandidates lists the documented install paths per platform.
func libreOfficeCandidates() []string {
	switch runtime.GOOS {
	case "windows":
		var out []string
		for _, root := range []string{
			os.Getenv("ProgramFiles"),
			os.Getenv("ProgramFiles(x86)"),
			os.Getenv("ProgramW6432"),
		} {
			if root == "" {
				continue
			}
			out = append(out, filepath.Join(root, "LibreOffice", "program", "soffice.exe"))
		}
		return out
	case "darwin":
		return []string{
			"/Applications/LibreOffice.app/Contents/MacOS/soffice",
			filepath.Join(os.Getenv("HOME"), "Applications", "LibreOffice.app", "Contents", "MacOS", "soffice"),
		}
	default:
		return []string{
			"/usr/bin/libreoffice",
			"/usr/bin/soffice",
			"/usr/local/bin/libreoffice",
			"/usr/local/bin/soffice",
			"/snap/bin/libreoffice",
			"/opt/libreoffice/program/soffice",
			"/usr/lib/libreoffice/program/soffice",
		}
	}
}
