package printer

import (
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/shiuoei/print-bridge/internal/domain"
)

// Capability buffers come from printer drivers, which are both third-party and
// historically loose with their own documented limits. Every allocation derived
// from a driver-returned count is clamped to these bounds first: a driver that
// reports (DWORD)-1 by mistake, or a garbage count, must not turn into a
// multi-gigabyte allocation.
const (
	// maxCapabilityEntries bounds papers, trays, media types and resolutions.
	// Real drivers report tens of entries at most (85 papers was the largest
	// printer seen on the development machine).
	maxCapabilityEntries = 4096
	// Fixed UTF-16 slot widths from the Win32 API contract.
	paperNameWidth = 64 // CCHDEVICENAME-style full name
	trayNameWidth  = 24 // DC_BINNAMES slot
	mediaNameWidth = 64 // DC_MEDIATYPENAMES slot
)

// capPoint mirrors the Win32 POINT layout (two LONGs). The unit depends on the
// call that filled it: DC_PAPERSIZE is tenths of a millimetre, while
// DC_ENUMRESOLUTIONS is dots per inch.
type capPoint struct{ X, Y int32 }

// newPrinterInfo builds an info value whose list members are non-nil so the
// protocol never emits `"papers": null`. Clients type these as arrays.
func newPrinterInfo(name string) *domain.PrinterInfo {
	return &domain.PrinterInfo{
		Name:       name,
		Papers:     []domain.PrinterPaper{},
		Trays:      []domain.PrinterTray{},
		MediaTypes: []domain.PrinterMediaType{},
	}
}

// capabilityCount turns a DeviceCapabilities return value into an element count.
//
// The Win32 contract reports failure/unsupported as (DWORD)-1, which arrives
// here as a negative int32. Treating that as a count is exactly the bug that
// made a naive caller try to allocate 549,755,813,888 bytes. Values above
// maxCapabilityEntries are equally untrustworthy and are rejected the same way.
// A zero return with ok=true means "supported, genuinely empty" (for example a
// PDF writer that declares no input slots).
func capabilityCount(raw int32) (int, bool) {
	if raw < 0 || int(raw) > maxCapabilityEntries {
		return 0, false
	}
	return int(raw), true
}

// fixedStrings decodes count fixed-width UTF-16 slots, each NUL-padded. It
// refuses out-of-range widths/counts and tolerates a short buffer by decoding
// only the slots actually present.
func fixedStrings(buf []uint16, width, count int) []string {
	if width <= 0 || count <= 0 || len(buf) < width {
		return nil
	}
	if max := len(buf) / width; count > max {
		count = max
	}
	out := make([]string, count)
	for i := 0; i < count; i++ {
		out[i] = decodeFixedUTF16(buf[i*width : (i+1)*width])
	}
	return out
}

// decodeFixedUTF16 decodes one NUL-terminated UTF-16 slot. utf16.Decode (rather
// than a hand-rolled scan) keeps surrogate pairs intact, which matters for
// Chinese paper names.
func decodeFixedUTF16(slot []uint16) string {
	if i := indexOfNUL(slot); i >= 0 {
		slot = slot[:i]
	}
	return strings.TrimSpace(string(utf16.Decode(slot)))
}

func indexOfNUL(slot []uint16) int {
	for i, v := range slot {
		if v == 0 {
			return i
		}
	}
	return -1
}

// tenthMMToMM converts the DC_PAPERSIZE unit (tenths of a millimetre) to the
// millimetres used by the wire protocol.
func tenthMMToMM(v int32) float64 { return float64(v) / 10 }

// decodePrinterPapers combines the three parallel DC_PAPERS/DC_PAPERNAMES/
// DC_PAPERSIZE arrays into protocol papers.
//
// Papers whose driver-reported size is not positive are dropped: the protocol
// requires width_mm/height_mm, and a 0x0 entry means the driver did not give a
// usable geometry. Inventing a default would make the capability list lie.
func decodePrinterPapers(ids []uint16, nameBuf []uint16, sizes []capPoint) []domain.PrinterPaper {
	// The three arrays are index-aligned: row i is (ids[i], name[i], sizes[i]).
	// A buffer that decodes to fewer names than there are ids cannot describe the
	// remaining rows, so the shortest array bounds the result. Names are optional
	// in the protocol, so when the name buffer is unusable entirely the sized rows
	// are still reported without names rather than dropped.
	names := fixedStrings(nameBuf, paperNameWidth, len(ids))
	n := min(len(ids), len(sizes))
	if len(names) > 0 {
		n = min(n, len(names))
	}
	if n <= 0 {
		return []domain.PrinterPaper{}
	}
	out := make([]domain.PrinterPaper, 0, n)
	for i := 0; i < n; i++ {
		if sizes[i].X <= 0 || sizes[i].Y <= 0 {
			continue
		}
		paper := domain.PrinterPaper{
			ID:       strconv.Itoa(int(ids[i])),
			WidthMM:  tenthMMToMM(sizes[i].X),
			HeightMM: tenthMMToMM(sizes[i].Y),
		}
		if i < len(names) {
			paper.Name = names[i]
		}
		out = append(out, paper)
	}
	return out
}

// decodePrinterTrays combines DC_BINS (WORD ids) with DC_BINNAMES.
func decodePrinterTrays(ids []uint16, nameBuf []uint16) []domain.PrinterTray {
	names := fixedStrings(nameBuf, trayNameWidth, len(ids))
	n := len(ids)
	if len(names) > 0 {
		n = min(n, len(names))
	}
	if n <= 0 {
		return []domain.PrinterTray{}
	}
	out := make([]domain.PrinterTray, 0, n)
	for i := 0; i < n; i++ {
		tray := domain.PrinterTray{ID: strconv.Itoa(int(ids[i]))}
		if i < len(names) {
			tray.Name = names[i]
		}
		out = append(out, tray)
	}
	return out
}

// decodePrinterMediaTypes combines DC_MEDIATYPES (DWORD ids) with
// DC_MEDIATYPENAMES. The id array really is DWORD: decoding it as WORD yields
// interleaved zeros and halves the names.
func decodePrinterMediaTypes(ids []uint32, nameBuf []uint16) []domain.PrinterMediaType {
	names := fixedStrings(nameBuf, mediaNameWidth, len(ids))
	n := len(ids)
	if len(names) > 0 {
		n = min(n, len(names))
	}
	if n <= 0 {
		return []domain.PrinterMediaType{}
	}
	out := make([]domain.PrinterMediaType, 0, n)
	for i := 0; i < n; i++ {
		media := domain.PrinterMediaType{ID: strconv.FormatUint(uint64(ids[i]), 10)}
		if i < len(names) {
			media.Name = names[i]
		}
		out = append(out, media)
	}
	return out
}

// DEVMODEW field offsets (bytes) used for resolution probing. The structure is
// a fixed Win32 layout, and these are the only members this package reads.
const (
	dmSizeOffset         = 68 // dmSize: WORD, size of the public DEVMODE
	dmFieldsOffset       = 72 // dmFields: DWORD
	dmPrintQualityOffset = 90 // dmPrintQuality: short
	dmYResolutionOffset  = 96 // dmYResolution: short
	// dmMinReadable is the smallest DEVMODE that still contains dmYResolution.
	dmMinReadable = dmYResolutionOffset + 2
)

// dmFields bits used for resolution probing.
const (
	dmPrintQualityField = 0x00040000
	dmYResolutionField  = 0x00020000
)

// decodeDevModeResolution reads the resolution out of a raw DEVMODEW image.
//
// It validates dmSize before trusting the trailing fields, because some drivers
// publish a shorter DEVMODE than the union implies and reading past its end
// would be reading unrelated heap memory.
func decodeDevModeResolution(blob []byte) *int {
	if len(blob) < dmMinReadable {
		return nil
	}
	if size := uint16(blob[dmSizeOffset]) | uint16(blob[dmSizeOffset+1])<<8; int(size) < dmMinReadable {
		return nil
	}
	fields := uint32(blob[dmFieldsOffset]) |
		uint32(blob[dmFieldsOffset+1])<<8 |
		uint32(blob[dmFieldsOffset+2])<<16 |
		uint32(blob[dmFieldsOffset+3])<<24
	printQuality := int16(uint16(blob[dmPrintQualityOffset]) | uint16(blob[dmPrintQualityOffset+1])<<8)
	yResolution := int16(uint16(blob[dmYResolutionOffset]) | uint16(blob[dmYResolutionOffset+1])<<8)
	return dpiFromDevMode(fields, printQuality, yResolution)
}

// dpiFromDevMode extracts the resolution the driver currently reports in its
// DEVMODE. dmPrintQuality is either a positive DPI value or one of the negative
// DMRES_* constants, so only positive values are usable.
func dpiFromDevMode(fields uint32, printQuality, yResolution int16) *int {
	if fields&dmPrintQualityField != 0 && printQuality > 0 {
		return intPtr(int(printQuality))
	}
	if fields&dmYResolutionField != 0 && yResolution > 0 {
		return intPtr(int(yResolution))
	}
	return nil
}

// dpiFromResolutions takes the first DC_ENUMRESOLUTIONS entry. Each entry is a
// {horizontal, vertical} DPI pair; the protocol carries a single number, so the
// horizontal component is reported.
func dpiFromResolutions(resolutions []capPoint) *int {
	for _, p := range resolutions {
		if p.X > 0 && p.X <= 100000 {
			return intPtr(int(p.X))
		}
	}
	return nil
}

// virtualPortMarkers and virtualDriverMarkers are strong positive evidence that
// a queue writes to a file rather than paper. They are matched case-insensitively
// as substrings of the port name or driver name.
var virtualPortMarkers = []string{
	"portprompt:", // Microsoft Print to PDF / XPS
	"file:",       // print to file
	"nul:",        // discard
	"shrFax:",     // fax driver
	"virtual",     // e.g. "Kingsoft Virtual Printer Port"
	"print to pdf", "print to xps",
}

var virtualDriverMarkers = []string{
	"virtual", "print to pdf", "print to xps", "pdf", "xps",
	"onenote", "document writer", "fax",
}

// physicalPortMarkers identify queues that own a real device. They are checked
// only after the virtual markers so a "Virtual USB port" still counts as virtual.
var physicalPortMarkers = []string{
	"usb", "lpt", "com", "wsd-", "wsd:", "ip_", "dns-", "dot4", "bth", "tcp",
	"hp", "socket", "network", "smb", "\\\\",
}

// virtualPrinterEvidence decides is_virtual from port and driver names.
//
// It returns true/false only on positive evidence and nil when neither side is
// recognizable, because "not a virtual printer" is a claim this code cannot
// prove for an unfamiliar driver. Verified against real queues on the
// development machine: "PORTPROMPT:" (Microsoft Print to PDF) and "Kingsoft
// Virtual Printer Port" are virtual; "USB001" (HPRT N41) and a "WSD-..." port
// (HP Laser MFP) are physical.
func virtualPrinterEvidence(port, driver string) *bool {
	p := strings.ToLower(port)
	d := strings.ToLower(driver)
	for _, marker := range virtualPortMarkers {
		if strings.Contains(p, strings.ToLower(marker)) {
			return boolPtr(true)
		}
	}
	for _, marker := range virtualDriverMarkers {
		if strings.Contains(d, marker) {
			return boolPtr(true)
		}
	}
	for _, marker := range physicalPortMarkers {
		if strings.Contains(p, strings.ToLower(marker)) {
			return boolPtr(false)
		}
	}
	return nil
}

// PRINTER_ATTRIBUTE_* flags that decide whether a queue is local. A printer is
// local when it is attached to this machine, and a network printer when the
// queue points at a remote share (or a non-local port). Several unrelated bits
// share the word, so only these are consulted.
const (
	printerAttributeNetwork     = 0x00000010 // PRINTER_ATTRIBUTE_NETWORK
	printerAttributeLocal       = 0x00000040 // PRINTER_ATTRIBUTE_LOCAL
	printerAttributeConnections = 0x00000002 // PRINTER_ATTRIBUTE_CONNECTIONS
)

// classifyWindowsAttributes maps PRINTER_INFO_2.dwAttributes plus the enumerated
// scope to is_local/is_network and returns the transport hints used for virtual
// detection.
//
// It returns nil for both when the attributes word carries no usable bit: an
// all-zero word means "no information supplied", not "remote physical device".
// PRINTER_ATTRIBUTE_LOCAL is the authoritative positive signal, so it is checked
// first; a connection-scoped queue without a local bit is treated as network.
func classifyWindowsAttributes(attributes uint32, enumeratedConnections bool) (isLocal, isNetwork *bool) {
	if attributes&printerAttributeLocal != 0 {
		return boolPtr(true), boolPtr(false)
	}
	if attributes&printerAttributeNetwork != 0 {
		return boolPtr(false), boolPtr(true)
	}
	if enumeratedConnections || attributes&printerAttributeConnections != 0 {
		return boolPtr(false), boolPtr(true)
	}
	return nil, nil
}

// CUPS device-URI schemes. CUPS reports "network" by whether the backend talks to
// another host, so the scheme is the only reliable signal available without
// extra tooling.
var cupsNetworkSchemes = map[string]bool{
	"ipp": true, "ipps": true, "http": true, "https": true, "socket": true,
	"lpd": true, "smb": true, "dnssd": true, "ldap": true, "rfc1738": true,
}

var cupsLocalSchemes = map[string]bool{
	"usb": true, "parallel": true, "serial": true, "usb-unix": true,
}

var cupsVirtualSchemes = map[string]bool{
	"cups-pdf": true, "file": true,
}

// classifyCUPSDevice maps a CUPS device URI to is_local/is_network. Unknown or
// absent URIs leave both nil rather than guessing.
func classifyCUPSDevice(uri string) (isLocal, isNetwork *bool) {
	scheme, rest := splitScheme(uri)
	if scheme == "" {
		return nil, nil
	}
	if cupsNetworkSchemes[scheme] {
		return boolPtr(false), boolPtr(true)
	}
	if cupsLocalSchemes[scheme] {
		return boolPtr(true), boolPtr(false)
	}
	if scheme == "hp" {
		// The HPLIP backend embeds the transport in the path: hp:/net/... vs
		// hp:/usb/....
		if strings.HasPrefix(rest, "/net") || strings.HasPrefix(rest, "//net") {
			return boolPtr(false), boolPtr(true)
		}
		if strings.HasPrefix(rest, "/usb") || strings.HasPrefix(rest, "//usb") {
			return boolPtr(true), boolPtr(false)
		}
	}
	return nil, nil
}

// virtualPrinterFromCUPS reports is_virtual for a CUPS URI. Only backends that
// are virtual by construction (PDF/file) are asserted; everything else stays
// unknown.
func virtualPrinterFromCUPS(uri string) *bool {
	scheme, _ := splitScheme(uri)
	if cupsVirtualSchemes[scheme] {
		return boolPtr(true)
	}
	if cupsNetworkSchemes[scheme] || cupsLocalSchemes[scheme] || scheme == "hp" {
		return boolPtr(false)
	}
	return nil
}

// splitScheme separates a URI scheme (lowercased, without "//") from the rest.
func splitScheme(uri string) (scheme, rest string) {
	i := strings.Index(uri, ":")
	if i <= 0 {
		return "", uri
	}
	scheme = strings.ToLower(strings.TrimSpace(uri[:i]))
	rest = uri[i+1:]
	return scheme, rest
}

// lpChoice is one selectable value of a CUPS printer option.
type lpChoice struct {
	Value   string
	Default bool
}

// lpOption is one parsed `lpoptions -p <printer> -l` entry, e.g.
// `PageSize/Media Size: 3x5 4x6 *A4 Letter`.
type lpOption struct {
	Key     string
	Choices []lpChoice
}

// parseLpoptions parses the `-l` listing defensively: the key is everything
// before the first colon, choices are whitespace separated, and a leading '*'
// marks the default. Malformed lines are skipped instead of aborting the parse,
// so one unexpected driver line cannot lose every capability.
func parseLpoptions(out string) []lpOption {
	var options []lpOption
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		colon := strings.IndexByte(line, ':')
		if colon <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:colon])
		if key == "" {
			continue
		}
		// Keep only the canonical side of "PageSize/Media Size".
		if slash := strings.IndexByte(key, '/'); slash > 0 {
			key = strings.TrimSpace(key[:slash])
		}
		option := lpOption{Key: key}
		for _, field := range strings.Fields(line[colon+1:]) {
			choice := lpChoice{Value: field}
			if strings.HasPrefix(field, "*") {
				choice.Default = true
				choice.Value = strings.TrimPrefix(field, "*")
			}
			if choice.Value == "" {
				continue
			}
			option.Choices = append(option.Choices, choice)
		}
		if len(option.Choices) > 0 {
			options = append(options, option)
		}
	}
	return options
}

// lpOptionValues returns the choices of the first option whose canonical key
// matches one of keys.
func lpOptionValues(options []lpOption, keys ...string) []lpChoice {
	for _, option := range options {
		for _, key := range keys {
			if strings.EqualFold(option.Key, key) {
				return option.Choices
			}
		}
	}
	return nil
}

// lpPapers derives papers from the PageSize option. Choices without a size this
// package can prove are dropped, so the list never contains invented dimensions.
func lpPapers(options []lpOption) []domain.PrinterPaper {
	choices := lpOptionValues(options, "PageSize", "media", "PageRegion")
	out := make([]domain.PrinterPaper, 0, len(choices))
	seen := make(map[string]bool, len(choices))
	for _, choice := range choices {
		paper, ok := lpPaperFromChoice(choice.Value)
		if !ok {
			continue
		}
		if seen[paper.ID] {
			continue
		}
		seen[paper.ID] = true
		out = append(out, paper)
	}
	return out
}

// lpPaperFromChoice resolves a CUPS PageSize choice. Two forms are provable:
// a well-known standard name (ISO/US sizes, whose dimensions are fixed by
// standard) and an explicit `Custom.WIDTHxHEIGHTmm`. Anything else - including
// the historical unitless Custom.WIDTHxHEIGHT in points - is rejected rather
// than guessed.
func lpPaperFromChoice(value string) (domain.PrinterPaper, bool) {
	raw := strings.TrimSpace(value)
	if raw == "" {
		return domain.PrinterPaper{}, false
	}
	lower := strings.ToLower(raw)
	if strings.HasPrefix(lower, "custom.") {
		if w, h, ok := parseCustomSizeMM(raw[len("custom."):]); ok {
			return sizedPaper(raw, w, h), true
		}
		return domain.PrinterPaper{}, false
	}
	base := raw
	if dot := strings.IndexByte(base, '.'); dot > 0 {
		// "A4.Borderless", "3x5.Borderless": keep the size part.
		base = base[:dot]
	}
	if w, h, ok := standardPaperSize(base); ok {
		return sizedPaper(raw, w, h), true
	}
	if w, h, ok := parseCustomSizeMM(base); ok {
		return sizedPaper(raw, w, h), true
	}
	return domain.PrinterPaper{}, false
}

func sizedPaper(id string, widthMM, heightMM float64) domain.PrinterPaper {
	return domain.PrinterPaper{ID: id, Name: id, WidthMM: widthMM, HeightMM: heightMM}
}

// parseCustomSizeMM parses the explicit millimetre form "100x150mm". A bare
// "100x150" is rejected because CUPS documents that form in points.
func parseCustomSizeMM(value string) (widthMM, heightMM float64, ok bool) {
	text := strings.ToLower(strings.TrimSpace(value))
	if !strings.HasSuffix(text, "mm") {
		return 0, 0, false
	}
	text = strings.TrimSuffix(text, "mm")
	x := strings.IndexByte(text, 'x')
	if x <= 0 || x == len(text)-1 {
		return 0, 0, false
	}
	w, errW := strconv.ParseFloat(strings.TrimSpace(text[:x]), 64)
	h, errH := strconv.ParseFloat(strings.TrimSpace(text[x+1:]), 64)
	if errW != nil || errH != nil || w <= 0 || h <= 0 || w > 2000 || h > 2000 {
		return 0, 0, false
	}
	return w, h, true
}

// standardPaperSizes holds ISO/US standard dimensions in millimetres. Only
// names whose size is defined by a published standard are listed, because these
// values are the entire justification for reporting a size at all.
var standardPaperSizes = map[string][2]float64{
	"A0": {841, 1189}, "A1": {594, 841}, "A2": {420, 594}, "A3": {297, 420},
	"A4": {210, 297}, "A5": {148, 210}, "A6": {105, 148}, "A7": {74, 105},
	"A8": {52, 74}, "A9": {37, 52}, "A10": {26, 37},
	"B0": {1000, 1414}, "B1": {707, 1000}, "B2": {500, 707}, "B3": {353, 500},
	"B4": {250, 353}, "B5": {176, 250}, "B6": {125, 176},
	"B4JIS": {257, 364}, "B5JIS": {182, 257},
	"C4": {229, 324}, "C5": {162, 229}, "C6": {114, 162}, "DL": {110, 220},
	"LETTER": {216, 279}, "LEGAL": {216, 356}, "TABLOID": {279, 432},
	"LEDGER": {432, 279}, "EXECUTIVE": {184, 267}, "STATEMENT": {140, 216},
	"FOLIO": {216, 330}, "GOVERNMENTLETTER": {203, 267},
	"4X6": {102, 152}, "5X7": {127, 178}, "3X5": {76, 127}, "NAINDEXCARD": {76, 127},
}

// standardPaperSize normalizes a CUPS choice ("B5 (JIS)", "us-letter", "A4.
// Borderless") and looks it up.
func standardPaperSize(name string) (widthMM, heightMM float64, ok bool) {
	key := strings.ToUpper(strings.TrimSpace(name))
	key = strings.NewReplacer(" ", "", "-", "", "_", "", "(", "", ")", "", "/", "").Replace(key)
	// CUPS names US media with a "US" infix ("us-letter") while PPD drivers use a
	// "na_"/"om_"/"pwg_" prefix; both normalize to the bare US size name.
	for _, prefix := range []string{"US", "NA", "OM", "PWG"} {
		if strings.HasPrefix(key, prefix) && len(key) > len(prefix) {
			if size, found := standardPaperSizes[key[len(prefix):]]; found {
				return size[0], size[1], true
			}
		}
	}
	if size, found := standardPaperSizes[key]; found {
		return size[0], size[1], true
	}
	return 0, 0, false
}

// lpTrays derives trays from the InputSlot option.
func lpTrays(options []lpOption) []domain.PrinterTray {
	choices := lpOptionValues(options, "InputSlot", "MediaSource")
	out := make([]domain.PrinterTray, 0, len(choices))
	seen := make(map[string]bool, len(choices))
	for _, choice := range choices {
		if seen[choice.Value] {
			continue
		}
		seen[choice.Value] = true
		out = append(out, domain.PrinterTray{ID: choice.Value, Name: choice.Value})
	}
	return out
}

// lpMediaTypes derives media types from the MediaType option.
func lpMediaTypes(options []lpOption) []domain.PrinterMediaType {
	choices := lpOptionValues(options, "MediaType")
	out := make([]domain.PrinterMediaType, 0, len(choices))
	seen := make(map[string]bool, len(choices))
	for _, choice := range choices {
		if seen[choice.Value] {
			continue
		}
		seen[choice.Value] = true
		out = append(out, domain.PrinterMediaType{ID: choice.Value, Name: choice.Value})
	}
	return out
}

// parseLpstatDevices parses `lpstat -v` output:
//
//	device for Office: ipp://print.local/printers/office
//
// CUPS printer names cannot contain ':', so the first colon after the prefix is
// the separator even when the URI contains colons of its own.
func parseLpstatDevices(out string) map[string]string {
	devices := make(map[string]string)
	const prefix = "device for "
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		trimmed := strings.TrimSpace(line)
		idx := strings.Index(strings.ToLower(trimmed), prefix)
		if idx < 0 {
			continue
		}
		rest := trimmed[idx+len(prefix):]
		colon := strings.IndexByte(rest, ':')
		if colon <= 0 {
			continue
		}
		name := strings.TrimSpace(rest[:colon])
		uri := strings.TrimSpace(rest[colon+1:])
		// A line without a URI proves nothing about the device and must not make
		// the queue look like it exists with an empty port.
		if name == "" || uri == "" {
			continue
		}
		devices[name] = uri
	}
	return devices
}

// parseLpstatDefault parses `lpstat -d`, which prints either
// "system default destination: Office" or "no system default destination".
func parseLpstatDefault(out string) string {
	for _, line := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(strings.TrimRight(line, "\r"))
		if trimmed == "" {
			continue
		}
		lower := strings.ToLower(trimmed)
		if strings.HasPrefix(lower, "no system default") {
			return ""
		}
		colon := strings.IndexByte(trimmed, ':')
		if colon < 0 {
			continue
		}
		if strings.Contains(strings.ToLower(trimmed[:colon]), "default") {
			return strings.TrimSpace(trimmed[colon+1:])
		}
	}
	return ""
}

// lookupDevice finds a CUPS device entry, preferring an exact name and falling
// back to a case-insensitive match.
func lookupDevice(devices map[string]string, name string) (string, bool) {
	if uri, ok := devices[name]; ok {
		return uri, true
	}
	for candidate, uri := range devices {
		if strings.EqualFold(candidate, name) {
			return uri, true
		}
	}
	return "", false
}

// unknownPrinterOutput reports whether lpoptions' output identifies a missing
// printer. CUPS prints e.g. `lpoptions: Unknown printer or class "x".` when run
// with LC_ALL=C.
func unknownPrinterOutput(out []byte) bool {
	message := strings.ToLower(string(out))
	for _, marker := range []string{"unknown printer", "no destination", "not found", "does not exist"} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}
