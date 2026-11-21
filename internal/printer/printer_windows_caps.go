//go:build windows

package printer

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unsafe"

	"github.com/shiuoei/print-bridge/internal/domain"
	"golang.org/x/sys/windows"
)

// DeviceCapabilitiesW capability ids. The values are the Win32 constants from
// wingdi.h; they are spelled out because x/sys/windows does not export them.
const (
	dcPapers          = 2  // WORD[] paper ids
	dcPaperSizes      = 3  // POINT[] size in tenths of a millimetre
	dcBins            = 6  // WORD[] tray ids
	dcBinNames        = 12 // fixed 24-WCHAR slots
	dcEnumResolutions = 13 // POINT[] {horizontal, vertical} DPI pairs
	dcPaperNames      = 16 // fixed 64-WCHAR slots
	dcMediaTypeNames  = 34 // fixed 64-WCHAR slots
	dcMediaTypes      = 35 // DWORD[] media type ids
)

// EnumPrinters scopes. Level 2 additionally yields port, driver, attributes and
// the DEVMODE, which is where the non-capability facts come from.
const (
	printerEnumLocal       = 0x00000002
	printerEnumConnections = 0x00000004
)

// maxEnumPrintersBuffer bounds the EnumPrintersW buffer. The largest real
// spooler installed here returns four queues; 32 MiB is far above anything a
// workstation reports and keeps a bogus pcbNeeded from becoming an allocation.
const maxEnumPrintersBuffer = 32 << 20

var (
	devCapabilities   = spool.NewProc("DeviceCapabilitiesW")
	getDefaultPrinter = spool.NewProc("GetDefaultPrinterW")
)

// winPrinterInfo2 mirrors the PRINTER_INFO_2W layout. Only the members this
// package reads are named; the offsets and order must stay exact because the
// structure is reinterpreted from the raw EnumPrinters buffer.
type winPrinterInfo2 struct {
	ServerName         *uint16
	PrinterName        *uint16
	ShareName          *uint16
	PortName           *uint16
	DriverName         *uint16
	Comment            *uint16
	Location           *uint16
	DevMode            *byte
	SepFile            *uint16
	PrintProcessor     *uint16
	Datatype           *uint16
	Parameters         *uint16
	SecurityDescriptor uintptr
	Attributes         uint32
	Priority           uint32
	DefaultPriority    uint32
	StartTime          uint32
	UntilTime          uint32
	Status             uint32
	Jobs               uint32
	AveragePPM         uint32
}

// windowsPrinterFacts is one queue's non-capability metadata.
type windowsPrinterFacts struct {
	Name       string
	Port       string
	Driver     string
	Attributes uint32
	DevModeDPI *int
}

// enumPrinterFacts lists queues with level 2 detail. The raw buffer is returned
// alongside the entries because PRINTER_INFO_2W points its string and DEVMODE
// members into it; the caller must keep it alive while reading them.
func enumPrinterFacts() ([]windowsPrinterFacts, *[]byte, error) {
	const scopes = printerEnumLocal | printerEnumConnections
	var needed, count uint32
	enumPrinters.Call(scopes, 0, 2, 0, 0, uintptr(unsafe.Pointer(&needed)), uintptr(unsafe.Pointer(&count)))
	if needed == 0 {
		return nil, nil, nil
	}
	if needed > maxEnumPrintersBuffer {
		return nil, nil, fmt.Errorf("spooler reported an implausible printer buffer size: %d bytes", needed)
	}
	buf := make([]byte, needed)
	ok, _, callErr := enumPrinters.Call(scopes, 0, 2, uintptr(unsafe.Pointer(&buf[0])), uintptr(needed), uintptr(unsafe.Pointer(&needed)), uintptr(unsafe.Pointer(&count)))
	if err := winErr(ok, callErr); err != nil {
		return nil, nil, err
	}
	if count == 0 {
		return nil, &buf, nil
	}
	entries := unsafe.Slice((*winPrinterInfo2)(unsafe.Pointer(&buf[0])), count)
	facts := make([]windowsPrinterFacts, 0, count)
	for i := range entries {
		entry := &entries[i]
		fact := windowsPrinterFacts{
			Name:       windows.UTF16PtrToString(entry.PrinterName),
			Port:       windows.UTF16PtrToString(entry.PortName),
			Driver:     windows.UTF16PtrToString(entry.DriverName),
			Attributes: entry.Attributes,
		}
		if entry.DevMode != nil {
			if blob := devModeSlice(&buf, entry.DevMode); blob != nil {
				fact.DevModeDPI = decodeDevModeResolution(blob)
			}
		}
		facts = append(facts, fact)
	}
	return facts, &buf, nil
}

// devModeSlice bounds the DEVMODE that a PRINTER_INFO_2W entry points at. The
// pointer must lie inside the buffer and the declared dmSize must fit, otherwise
// the pointer is a stale driver value and reading it would be out of bounds.
func devModeSlice(buf *[]byte, dm *byte) []byte {
	if dm == nil || len(*buf) == 0 {
		return nil
	}
	base := uintptr(unsafe.Pointer(&(*buf)[0]))
	addr := uintptr(unsafe.Pointer(dm))
	size := uintptr(len(*buf))
	if addr < base || addr-base >= size {
		return nil
	}
	offset := addr - base
	// dmSize (WORD at offset 68) plus driver-extra bytes; cap at 8 KiB, which
	// is far above any real DEVMODE.
	remaining := size - offset
	if remaining < dmMinReadable {
		return nil
	}
	limit := remaining
	if limit > 8192 {
		limit = 8192
	}
	blob := (*buf)[offset : offset+limit]
	if declared := uintptr(uint16(blob[dmSizeOffset]) | uint16(blob[dmSizeOffset+1])<<8); declared > limit && declared <= remaining {
		blob = (*buf)[offset : offset+declared]
	}
	return blob
}

// deviceCapability issues one DeviceCapabilitiesW call. Every caller goes
// through it so pOutput is always nil (count query) or a validated buffer.
//
// The signature has five parameters; omitting pDevMode shifts the stack and
// returns garbage, which is why the call site passes all of them explicitly.
func deviceCapability(printerName, portName string, capability uintptr, output unsafe.Pointer) int32 {
	namePtr, err := windows.UTF16PtrFromString(printerName)
	if err != nil {
		return -1
	}
	// An empty port name makes the spooler resolve the port for this queue.
	portPtr, err := windows.UTF16PtrFromString(portName)
	if err != nil {
		return -1
	}
	ret, _, _ := devCapabilities.Call(
		uintptr(unsafe.Pointer(namePtr)),
		uintptr(unsafe.Pointer(portPtr)),
		capability,
		uintptr(output),
		0, // pDevMode: NULL means "use the printer's current defaults"
	)
	return int32(uint32(ret))
}

// capabilityCountOf performs the count query and applies the shared bounds.
func capabilityCountOf(printerName, portName string, capability uintptr) (int, bool) {
	return capabilityCount(deviceCapability(printerName, portName, capability, nil))
}

// Info reports the capabilities of one Windows printer.
func (platformBackend) Info(ctx context.Context, name string) (*domain.PrinterInfo, error) {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return nil, fmt.Errorf("printer name required: %w", ErrPrinterNotFound)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	facts, rawBuf, err := enumPrinterFacts()
	if err != nil {
		return nil, err
	}
	_ = rawBuf
	var match *windowsPrinterFacts
	for i := range facts {
		if strings.EqualFold(facts[i].Name, trimmed) {
			match = &facts[i]
			break
		}
	}
	if match == nil {
		// Existence is decided by the spooler's own queue list. A driver that
		// answers DeviceCapabilities for an unknown name (or -1, which is
		// indistinguishable from "unsupported capability") must never turn into a
		// half-populated printer record.
		return nil, fmt.Errorf("%q: %w", trimmed, ErrPrinterNotFound)
	}

	info := newPrinterInfo(match.Name)
	info.Port = stringPtrOrNil(match.Port)
	info.IsVirtual = virtualPrinterEvidence(match.Port, match.Driver)
	info.IsLocal, info.IsNetwork = classifyWindowsAttributes(match.Attributes, false)
	info.Papers = windowsPapers(match.Name, match.Port)
	info.Trays = windowsTrays(match.Name, match.Port)
	info.MediaTypes = windowsMediaTypes(match.Name, match.Port)
	info.DPI = windowsDPI(match, match.Port)

	if def, err := (platformBackend{}).SystemDefault(ctx); err == nil && def != "" {
		info.IsDefault = boolPtr(strings.EqualFold(def, match.Name))
	}
	return info, nil
}

// stringPtrOrNil keeps "unknown port" out of the wire as an empty string.
func stringPtrOrNil(value string) *string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return stringPtr(value)
}

// windowsPapers reads DC_PAPERS, DC_PAPERNAMES and DC_PAPERSIZE.
//
// The three arrays are independent queries, so a driver can report a different
// count for each; the shortest is used. Any of them returning -1 means the
// capability is unsupported and papers stays an empty list.
func windowsPapers(printerName, portName string) []domain.PrinterPaper {
	paperCount, ok := capabilityCountOf(printerName, portName, dcPapers)
	if !ok || paperCount == 0 {
		return []domain.PrinterPaper{}
	}
	nameCount, ok := capabilityCountOf(printerName, portName, dcPaperNames)
	if !ok || nameCount == 0 {
		return []domain.PrinterPaper{}
	}
	count := min(paperCount, nameCount)
	// DC_PAPERSIZE is queried for the paper count only; a driver that cannot
	// size its papers still gets names, but without sizes the protocol has
	// nothing valid to report, so it is skipped below.
	sizeCount, ok := capabilityCountOf(printerName, portName, dcPaperSizes)
	if !ok || sizeCount == 0 {
		return []domain.PrinterPaper{}
	}
	count = min(count, sizeCount)

	ids := make([]uint16, count)
	if ret := deviceCapability(printerName, portName, dcPapers, unsafe.Pointer(&ids[0])); ret < 0 {
		return []domain.PrinterPaper{}
	}
	nameBuf := make([]uint16, count*paperNameWidth)
	if ret := deviceCapability(printerName, portName, dcPaperNames, unsafe.Pointer(&nameBuf[0])); ret < 0 {
		return []domain.PrinterPaper{}
	}
	sizes := make([]capPoint, count)
	if ret := deviceCapability(printerName, portName, dcPaperSizes, unsafe.Pointer(&sizes[0])); ret < 0 {
		return []domain.PrinterPaper{}
	}
	return dedupePapers(decodePrinterPapers(ids, nameBuf, sizes))
}

// windowsTrays reads DC_BINS and DC_BINNAMES.
func windowsTrays(printerName, portName string) []domain.PrinterTray {
	binCount, ok := capabilityCountOf(printerName, portName, dcBins)
	if !ok || binCount == 0 {
		return []domain.PrinterTray{}
	}
	nameCount, ok := capabilityCountOf(printerName, portName, dcBinNames)
	if !ok || nameCount == 0 {
		return []domain.PrinterTray{}
	}
	count := min(binCount, nameCount)
	ids := make([]uint16, count)
	if ret := deviceCapability(printerName, portName, dcBins, unsafe.Pointer(&ids[0])); ret < 0 {
		return []domain.PrinterTray{}
	}
	nameBuf := make([]uint16, count*trayNameWidth)
	if ret := deviceCapability(printerName, portName, dcBinNames, unsafe.Pointer(&nameBuf[0])); ret < 0 {
		return []domain.PrinterTray{}
	}
	return decodePrinterTrays(ids, nameBuf)
}

// windowsMediaTypes reads DC_MEDIATYPES and DC_MEDIATYPENAMES. Most drivers do
// not implement these and answer -1 (unsupported), which stays an empty list.
func windowsMediaTypes(printerName, portName string) []domain.PrinterMediaType {
	mediaCount, ok := capabilityCountOf(printerName, portName, dcMediaTypes)
	if !ok || mediaCount == 0 {
		return []domain.PrinterMediaType{}
	}
	nameCount, ok := capabilityCountOf(printerName, portName, dcMediaTypeNames)
	if !ok || nameCount == 0 {
		return []domain.PrinterMediaType{}
	}
	count := min(mediaCount, nameCount)
	// DC_MEDIATYPES fills DWORD ids; decoding them as WORD interleaves zeros.
	ids := make([]uint32, count)
	if ret := deviceCapability(printerName, portName, dcMediaTypes, unsafe.Pointer(&ids[0])); ret < 0 {
		return []domain.PrinterMediaType{}
	}
	nameBuf := make([]uint16, count*mediaNameWidth)
	if ret := deviceCapability(printerName, portName, dcMediaTypeNames, unsafe.Pointer(&nameBuf[0])); ret < 0 {
		return []domain.PrinterMediaType{}
	}
	return decodePrinterMediaTypes(ids, nameBuf)
}

// windowsDPI reports the queue resolution, preferring the DEVMODE the spooler
// already returned (that is the current configuration) and falling back to the
// driver's supported-resolution list.
func windowsDPI(facts *windowsPrinterFacts, portName string) *int {
	if facts.DevModeDPI != nil {
		return facts.DevModeDPI
	}
	count, ok := capabilityCountOf(facts.Name, portName, dcEnumResolutions)
	if !ok || count == 0 {
		return nil
	}
	resolutions := make([]capPoint, count)
	if ret := deviceCapability(facts.Name, portName, dcEnumResolutions, unsafe.Pointer(&resolutions[0])); ret < 0 {
		return nil
	}
	return dpiFromResolutions(resolutions)
}

// dedupePapers removes entries with identical id+name+size. Drivers frequently
// list the same geometry several times, and the UI would show duplicate rows.
func dedupePapers(papers []domain.PrinterPaper) []domain.PrinterPaper {
	if len(papers) == 0 {
		return []domain.PrinterPaper{}
	}
	seen := make(map[string]bool, len(papers))
	out := make([]domain.PrinterPaper, 0, len(papers))
	for _, paper := range papers {
		key := fmt.Sprintf("%s|%s|%.1f|%.1f", paper.ID, paper.Name, paper.WidthMM, paper.HeightMM)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, paper)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// SystemDefault returns the operating system's default printer.
func (platformBackend) SystemDefault(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	var size uint32
	getDefaultPrinter.Call(0, uintptr(unsafe.Pointer(&size)))
	if size == 0 {
		return "", nil
	}
	buf := make([]uint16, size)
	ok, _, callErr := getDefaultPrinter.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)))
	if err := winErr(ok, callErr); err != nil {
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_INSUFFICIENT_BUFFER) {
			return "", nil
		}
		return "", err
	}
	return windows.UTF16ToString(buf), nil
}
