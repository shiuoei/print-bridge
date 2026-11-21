//go:build !windows

package command

// firstOfficeProgID is Windows-only; other platforms rely on LibreOffice.
func firstOfficeProgID(string) string { return "" }
