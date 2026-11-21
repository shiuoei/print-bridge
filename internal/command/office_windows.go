//go:build windows

package command

import (
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

const (
	hkeyClassesRoot = 0x80000000
	keyQueryValue   = 0x0001
)

var (
	advapi32        = syscall.NewLazyDLL("advapi32.dll")
	regOpenKeyExW   = advapi32.NewProc("RegOpenKeyExW")
	regCloseKey     = advapi32.NewProc("RegCloseKey")
	regQueryValueEx = advapi32.NewProc("RegQueryValueExW")
)

// registryValueExists reports whether a value exists under the given registry
// key. It is a passive lookup: it never activates a COM server.
func registryValueExists(root uintptr, subKey, valueName string) bool {
	sub, err := syscall.UTF16PtrFromString(subKey)
	if err != nil {
		return false
	}
	var hkey syscall.Handle
	rc, _, _ := regOpenKeyExW.Call(root, uintptr(unsafe.Pointer(sub)), 0, keyQueryValue, uintptr(unsafe.Pointer(&hkey)))
	if rc != 0 {
		return false
	}
	defer regCloseKey.Call(uintptr(hkey))
	name, err := syscall.UTF16PtrFromString(valueName)
	if err != nil {
		return false
	}
	var typ, size uint32
	rc, _, _ = regQueryValueEx.Call(uintptr(hkey), uintptr(unsafe.Pointer(name)), 0, uintptr(unsafe.Pointer(&typ)), 0, uintptr(unsafe.Pointer(&size)))
	return rc == 0
}

// firstOfficeProgID returns the first registered converter ProgID for a format.
// A ProgID that is not registered means that Office flavour is not installed, so
// doctor must not report it as available.
func firstOfficeProgID(format string) string {
	for _, progID := range officeProgramNames(format) {
		// A registered automation server has an empty default value under its
		// CLSID key; the COM routing itself is proven by the ProgID->CLSID entry.
		if registryValueExists(hkeyClassesRoot, progID+`\CLSID`, "") {
			return progID
		}
	}
	return ""
}

var _ = filepath.Join
var _ = strings.ToLower
