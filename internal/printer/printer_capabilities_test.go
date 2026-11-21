package printer

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/shiuoei/print-bridge/internal/domain"
)

// fixedSlot encodes one NUL-padded UTF-16 slot of width WCHARs.
func fixedSlot(text string, width int) []uint16 {
	slot := make([]uint16, width)
	copy(slot, utf16.Encode([]rune(text)))
	return slot
}

// paperNameBuffer builds a DC_PAPERNAMES-shaped buffer (count * 64 WCHARs).
func paperNameBuffer(names ...string) []uint16 {
	out := make([]uint16, 0, len(names)*paperNameWidth)
	for _, name := range names {
		out = append(out, fixedSlot(name, paperNameWidth)...)
	}
	return out
}

// TestDecodePrinterPapers covers the real HPRT N41 rows observed on the
// development machine, plus the tenths-of-a-millimetre conversion.
func TestDecodePrinterPapers(t *testing.T) {
	ids := []uint16{256, 257, 258}
	names := paperNameBuffer("Custom(76.0mm * 127.0mm)", "76mm * 130mm", "菜鸟标准 100mm * 180mm")
	sizes := []capPoint{{760, 1270}, {760, 1270}, {1000, 1800}}

	got := decodePrinterPapers(ids, names, sizes)
	want := []domain.PrinterPaper{
		{ID: "256", Name: "Custom(76.0mm * 127.0mm)", WidthMM: 76, HeightMM: 127},
		{ID: "257", Name: "76mm * 130mm", WidthMM: 76, HeightMM: 127},
		{ID: "258", Name: "菜鸟标准 100mm * 180mm", WidthMM: 100, HeightMM: 180},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("decodePrinterPapers mismatch\n got: %#v\nwant: %#v", got, want)
	}
}

// TestDecodePrinterPapersDropsUnsizedRows proves a driver entry without a usable
// geometry is dropped rather than reported as a 0x0 paper.
func TestDecodePrinterPapersDropsUnsizedRows(t *testing.T) {
	ids := []uint16{1, 2, 3}
	names := paperNameBuffer("Good", "Zero", "Negative")
	sizes := []capPoint{{2100, 2970}, {0, 0}, {-5, 100}}

	got := decodePrinterPapers(ids, names, sizes)
	if len(got) != 1 || got[0].Name != "Good" {
		t.Fatalf("expected only the sized paper, got %#v", got)
	}
	if got[0].WidthMM != 210 || got[0].HeightMM != 297 {
		t.Fatalf("A4 conversion wrong: %#v", got[0])
	}
}

// TestDecodePrinterPapersShortBuffers proves mismatched array lengths cannot
// panic: the shortest array decides.
func TestDecodePrinterPapersShortBuffers(t *testing.T) {
	ids := []uint16{1, 2, 3, 4}
	names := paperNameBuffer("OnlyOne")
	sizes := []capPoint{{100, 200}, {300, 400}}

	got := decodePrinterPapers(ids, names, sizes)
	if len(got) != 1 {
		t.Fatalf("expected 1 paper (min of 4/1/2), got %d: %#v", len(got), got)
	}
	if got[0].Name != "OnlyOne" || got[0].WidthMM != 10 || got[0].HeightMM != 20 {
		t.Fatalf("unexpected row: %#v", got[0])
	}
}

// TestDecodePrinterPapersEmptyAndNil proves nil/empty inputs yield a non-nil
// empty slice so the wire never carries "papers": null.
func TestDecodePrinterPapersEmptyAndNil(t *testing.T) {
	for name, got := range map[string][]domain.PrinterPaper{
		"nil ids":   decodePrinterPapers(nil, nil, nil),
		"empty ids": decodePrinterPapers([]uint16{}, []uint16{}, []capPoint{}),
	} {
		if got == nil {
			t.Fatalf("%s: expected non-nil slice", name)
		}
		if len(got) != 0 {
			t.Fatalf("%s: expected empty slice, got %#v", name, got)
		}
	}
}

// TestDecodePrinterTrays covers the real "Roll Paper Feeder" row and the 24-WCHAR
// DC_BINNAMES slot width.
func TestDecodePrinterTrays(t *testing.T) {
	ids := []uint16{1, 7}
	names := make([]uint16, 0, 2*trayNameWidth)
	names = append(names, fixedSlot("Roll Paper Feeder", trayNameWidth)...)
	names = append(names, fixedSlot("自动选择", trayNameWidth)...)

	got := decodePrinterTrays(ids, names)
	want := []domain.PrinterTray{{ID: "1", Name: "Roll Paper Feeder"}, {ID: "7", Name: "自动选择"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("decodePrinterTrays mismatch\n got: %#v\nwant: %#v", got, want)
	}
}

// TestDecodePrinterMediaTypesUsesDWORDIDs is the regression guard for the real HP
// driver answer, whose ids are {0,1,10,11,2,7,4,5,8,6,9}. Decoding the buffer as
// WORD instead of DWORD yields interleaved zeros and wrong names.
func TestDecodePrinterMediaTypesUsesDWORDIDs(t *testing.T) {
	ids := []uint32{0, 1, 10, 11, 2, 7, 4, 5, 8, 6, 9}
	names := make([]uint16, 0, len(ids)*mediaNameWidth)
	for _, name := range []string{
		"未指定", "普通纸", "重磅纸 90-120g", "轻磅纸 60-69g", "证券纸", "彩纸",
		"超级重磅纸 121-163g", "标签纸", "信封", "预打印纸", "再生纸",
	} {
		names = append(names, fixedSlot(name, mediaNameWidth)...)
	}

	got := decodePrinterMediaTypes(ids, names)
	if len(got) != len(ids) {
		t.Fatalf("expected %d media types, got %d", len(ids), len(got))
	}
	if got[2].ID != "10" || got[2].Name != "重磅纸 90-120g" {
		t.Fatalf("id/name pairing wrong at index 2: %#v", got[2])
	}
	if got[0].ID != "0" || got[0].Name != "未指定" {
		t.Fatalf("unexpected first row: %#v", got[0])
	}
}

// TestDecodeFixedUTF16 handles empty, unterminated and whitespace-padded slots.
func TestDecodeFixedUTF16(t *testing.T) {
	cases := map[string]struct {
		slot []uint16
		want string
	}{
		"terminated":   {[]uint16{'A', 0, 'B', 0}, "A"},
		"unterminated": {[]uint16{'A', '4'}, "A4"},
		"padding":      {[]uint16{' ', 'A', '4', ' '}, "A4"},
		"all nul":      {make([]uint16, 8), ""},
		"empty":        {nil, ""},
	}
	for name, tc := range cases {
		if got := decodeFixedUTF16(tc.slot); got != tc.want {
			t.Fatalf("%s: got %q, want %q", name, got, tc.want)
		}
	}
}

// TestDecodeFixedUTF16SurrogatePair proves astral characters survive; a naive
// byte scan would split the pair.
func TestDecodeFixedUTF16SurrogatePair(t *testing.T) {
	slot := fixedSlot("纸📄", paperNameWidth)
	if got := decodeFixedUTF16(slot); got != "纸📄" {
		t.Fatalf("got %q, want %q", got, "纸📄")
	}
}

// TestCapabilityCountRejectsUnsupported is the regression guard for the
// VirtualAlloc crash: (DWORD)-1 arrives as -1 and must never become a length.
func TestCapabilityCountRejectsUnsupported(t *testing.T) {
	cases := []struct {
		raw    int32
		want   int
		wantOK bool
	}{
		{-1, 0, false}, // (DWORD)-1: unsupported
		{0, 0, true},   // supported, empty
		{1, 1, true},   // one tray
		{85, 85, true}, // real HPRT paper count
		{maxCapabilityEntries, maxCapabilityEntries, true},
		{maxCapabilityEntries + 1, 0, false}, // implausible
		{1 << 20, 0, false},                  // garbage
		{-2147483648, 0, false},              // INT32_MIN
	}
	for _, tc := range cases {
		got, ok := capabilityCount(tc.raw)
		if got != tc.want || ok != tc.wantOK {
			t.Fatalf("capabilityCount(%d) = (%d, %v), want (%d, %v)", tc.raw, got, ok, tc.want, tc.wantOK)
		}
	}
}

// TestCapabilityCountBoundsAllocation pins the exact number the naive caller blew
// up on: 0xFFFFFFFF must not reach make([]T, n).
func TestCapabilityCountBoundsAllocation(t *testing.T) {
	// A variable keeps this a runtime conversion: int32(uint32(0xFFFFFFFF)) as a
	// constant expression is a compile-time overflow, which is exactly why the
	// Win32 (DWORD)-1 sentinel is easy to mishandle.
	var unsupported uint32 = 0xFFFFFFFF
	raw := int32(unsupported)
	n, ok := capabilityCount(raw)
	if ok {
		t.Fatalf("0xFFFFFFFF must be treated as unsupported, got (%d, %v)", n, ok)
	}
	if n != 0 {
		t.Fatalf("unsupported capability must yield 0 entries, got %d", n)
	}
}

// TestFixedStringsGuards covers the allocation surface of fixedStrings: bogus
// widths and counts are rejected instead of panicking.
func TestFixedStringsGuards(t *testing.T) {
	buf := paperNameBuffer("A4", "Letter")
	if got := fixedStrings(buf, 0, 2); got != nil {
		t.Fatalf("zero width must be rejected, got %#v", got)
	}
	if got := fixedStrings(buf, paperNameWidth, 0); got != nil {
		t.Fatalf("zero count must be rejected, got %#v", got)
	}
	if got := fixedStrings(nil, paperNameWidth, 2); got != nil {
		t.Fatalf("nil buffer must be rejected, got %#v", got)
	}
	// Clamp: only two full slots exist, asking for more decodes what is there.
	got := fixedStrings(buf, paperNameWidth, 100)
	if len(got) != 2 || got[0] != "A4" || got[1] != "Letter" {
		t.Fatalf("clamped decode wrong: %#v", got)
	}
}

// TestDecodeDevModeResolution builds a DEVMODEW image byte by byte and checks the
// dmFields-gated DPI read.
func TestDecodeDevModeResolution(t *testing.T) {
	build := func(dmSize uint16, fields uint32, printQuality, yResolution int16) []byte {
		blob := make([]byte, 220)
		binary.LittleEndian.PutUint16(blob[dmSizeOffset:], dmSize)
		binary.LittleEndian.PutUint32(blob[dmFieldsOffset:], fields)
		binary.LittleEndian.PutUint16(blob[dmPrintQualityOffset:], uint16(printQuality))
		binary.LittleEndian.PutUint16(blob[dmYResolutionOffset:], uint16(yResolution))
		return blob
	}

	cases := []struct {
		name string
		blob []byte
		want *int
	}{
		// Real HPRT N41 DEVMODE: dmPrintQuality=203, dmYResolution=203.
		{"print quality", build(220, dmPrintQualityField, 203, 203), intPtr(203)},
		// Real HP Laser MFP: dmPrintQuality=600, dmYResolution=0.
		{"y resolution fallback", build(220, dmYResolutionField, 0, 600), intPtr(600)},
		// No resolution field set: unknown, not zero.
		{"no field", build(220, 0, 600, 600), nil},
		// DMRES_* constants are negative and unusable.
		{"negative quality", build(220, dmPrintQualityField, -3, 0), nil},
		// dmSize shorter than dmYResolution: refuse to read trailing bytes.
		{"short devmode", build(10, dmPrintQualityField, 600, 600), nil},
		{"truncated blob", make([]byte, dmMinReadable-1), nil},
	}
	for _, tc := range cases {
		if got := decodeDevModeResolution(tc.blob); !sameIntPtr(got, tc.want) {
			t.Fatalf("%s: got %v, want %v", tc.name, deref(got), deref(tc.want))
		}
	}
}

func sameIntPtr(a, b *int) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func deref(v *int) any {
	if v == nil {
		return nil
	}
	return *v
}

// TestDpiFromResolutions covers the DC_ENUMRESOLUTIONS fallback, including the
// real WPS driver list {600, 2400, 1200, ...} whose first entry is reported.
func TestDpiFromResolutions(t *testing.T) {
	if got := dpiFromResolutions([]capPoint{{600, 600}, {2400, 2400}}); !sameIntPtr(got, intPtr(600)) {
		t.Fatalf("expected first resolution 600, got %v", deref(got))
	}
	if got := dpiFromResolutions([]capPoint{{0, 0}, {203, 203}}); !sameIntPtr(got, intPtr(203)) {
		t.Fatalf("expected to skip the zero entry, got %v", deref(got))
	}
	if got := dpiFromResolutions(nil); got != nil {
		t.Fatalf("expected nil, got %v", deref(got))
	}
}

// TestVirtualPrinterEvidence pins the real-machine classifications: the two PDF
// writers are virtual, the USB ticket printer and the WSD multifunction are not.
func TestVirtualPrinterEvidence(t *testing.T) {
	cases := []struct {
		name   string
		port   string
		driver string
		want   *bool
	}{
		{"microsoft pdf", "PORTPROMPT:", "Microsoft Print To PDF", boolPtr(true)},
		{"wps pdf", "Kingsoft Virtual Printer Port", "Kingsoft Virtual Printer Driver", boolPtr(true)},
		{"hprt usb", "USB001", "HPRT N41", boolPtr(false)},
		{"hp wsd", "WSD-3af3f022-eba1-4369-9218-30809d18775d", "HP Laser MFP 131 133 135-138", boolPtr(false)},
		{"unknown", "", "Some Unknown Driver", nil},
	}
	for _, tc := range cases {
		got := virtualPrinterEvidence(tc.port, tc.driver)
		if got == nil || tc.want == nil {
			if got != tc.want {
				t.Fatalf("%s: got %v, want %v", tc.name, boolText(got), boolText(tc.want))
			}
			continue
		}
		if *got != *tc.want {
			t.Fatalf("%s: got %v, want %v", tc.name, *got, *tc.want)
		}
	}
}

func boolText(v *bool) string {
	if v == nil {
		return "nil"
	}
	if *v {
		return "true"
	}
	return "false"
}

// TestClassifyWindowsAttributes covers the real attribute words seen here:
// 0x240 and 0xA40 both mean local; a connection without the local bit is network.
func TestClassifyWindowsAttributes(t *testing.T) {
	cases := []struct {
		name       string
		attributes uint32
		local, net *bool
	}{
		{"hprt local", 0x00000a40, boolPtr(true), boolPtr(false)},
		{"microsoft pdf local", 0x00000240, boolPtr(true), boolPtr(false)},
		{"wps local", 0x00000041, boolPtr(true), boolPtr(false)},
		{"network bit", 0x00000010, boolPtr(false), boolPtr(true)},
		{"no information", 0x00000000, nil, nil},
	}
	for _, tc := range cases {
		local, net := classifyWindowsAttributes(tc.attributes, false)
		if !sameBoolPtr(local, tc.local) || !sameBoolPtr(net, tc.net) {
			t.Fatalf("%s: got (%s,%s), want (%s,%s)", tc.name, boolText(local), boolText(net), boolText(tc.local), boolText(tc.net))
		}
	}
	// A connection-scoped enumeration without the local bit is remote.
	local, net := classifyWindowsAttributes(0, true)
	if !sameBoolPtr(local, boolPtr(false)) || !sameBoolPtr(net, boolPtr(true)) {
		t.Fatalf("connection scope: got (%s,%s)", boolText(local), boolText(net))
	}
}

func sameBoolPtr(a, b *bool) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// TestParseLpoptions uses a realistic `lpoptions -p x -l` listing, including a
// malformed line that must be skipped instead of aborting the parse.
func TestParseLpoptions(t *testing.T) {
	out := strings.Join([]string{
		"PageSize/Media Size: 3x5 4x6 5x7 *A4 Letter Legal",
		"InputSlot/Media Source: *Auto Tray1 Tray2",
		"MediaType/Media Type: *Plain Glossy",
		"Resolution/Output Resolution: 300dpi *600dpi 1200dpi",
		"",
		"   ",
		"garbage line without a colon",
		"Broken:",
		"ColorModel/Color Mode: *Gray",
	}, "\n")

	options := parseLpoptions(out)
	keys := make([]string, 0, len(options))
	for _, option := range options {
		keys = append(keys, option.Key)
	}
	want := []string{"PageSize", "InputSlot", "MediaType", "Resolution", "ColorModel"}
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("keys = %#v, want %#v (malformed lines must be skipped)", keys, want)
	}
	if got := lpOptionValues(options, "PageSize"); len(got) != 6 {
		t.Fatalf("expected 6 PageSize choices, got %#v", got)
	}
	defaults := 0
	for _, choice := range lpOptionValues(options, "PageSize") {
		if choice.Default {
			defaults++
			if choice.Value != "A4" {
				t.Fatalf("default should be A4, got %q", choice.Value)
			}
		}
	}
	if defaults != 1 {
		t.Fatalf("expected exactly one default choice, got %d", defaults)
	}
}

// TestParseLpoptionsSlashLabel proves the canonical key is taken from
// "PageSize/Media Size" so callers can look it up by the short name.
func TestParseLpoptionsSlashLabel(t *testing.T) {
	options := parseLpoptions("PageSize/Media Size: A4\n")
	if len(options) != 1 || options[0].Key != "PageSize" {
		t.Fatalf("expected canonical key PageSize, got %#v", options)
	}
}

// TestParseLpoptionsCRLF proves a CRLF listing (lpoptions on some platforms) does
// not leave \r glued to the last choice.
func TestParseLpoptionsCRLF(t *testing.T) {
	options := parseLpoptions("InputSlot/Media Source: Tray1 *Tray2\r\nMediaType/Media Type: *Plain\r\n")
	slots := lpOptionValues(options, "InputSlot")
	if len(slots) != 2 || slots[1].Value != "Tray2" || !slots[1].Default {
		t.Fatalf("CRLF parse wrong: %#v", slots)
	}
	media := lpOptionValues(options, "MediaType")
	if len(media) != 1 || media[0].Value != "Plain" {
		t.Fatalf("CRLF parse wrong for MediaType: %#v", media)
	}
}

// TestLpPapersNeverFabricates proves only provable sizes produce papers.
func TestLpPapersNeverFabricates(t *testing.T) {
	options := parseLpoptions(strings.Join([]string{
		"PageSize/Media Size: *A4 Letter ObscureSize Custom.100x150mm Custom.100x150",
		"InputSlot/Media Source: *Auto",
	}, "\n"))

	papers := lpPapers(options)
	byID := make(map[string]domain.PrinterPaper, len(papers))
	for _, paper := range papers {
		byID[paper.ID] = paper
	}
	if _, ok := byID["ObscureSize"]; ok {
		t.Fatalf("unknown size must not be reported: %#v", papers)
	}
	// Bare Custom.100x150 is points, not millimetres: not provable.
	if _, ok := byID["Custom.100x150"]; ok {
		t.Fatalf("unitless Custom size must not be reported in mm: %#v", papers)
	}
	a4, ok := byID["A4"]
	if !ok || a4.WidthMM != 210 || a4.HeightMM != 297 {
		t.Fatalf("A4 must be 210x297mm, got %#v", a4)
	}
	letter, ok := byID["Letter"]
	if !ok || letter.WidthMM != 216 || letter.HeightMM != 279 {
		t.Fatalf("Letter must be 216x279mm, got %#v", letter)
	}
	custom, ok := byID["Custom.100x150mm"]
	if !ok || custom.WidthMM != 100 || custom.HeightMM != 150 {
		t.Fatalf("explicit mm custom size must parse, got %#v", custom)
	}
	if len(papers) != 3 {
		t.Fatalf("expected exactly A4, Letter and Custom.100x150mm, got %#v", papers)
	}
}

// TestLpPapersEmpty proves a listing without PageSize yields a non-nil empty
// slice.
func TestLpPapersEmpty(t *testing.T) {
	papers := lpPapers(parseLpoptions("InputSlot/Media Source: *Auto\n"))
	if papers == nil {
		t.Fatal("expected non-nil empty slice")
	}
	if len(papers) != 0 {
		t.Fatalf("expected no papers, got %#v", papers)
	}
}

// TestStandardPaperSize covers the normalization rules.
func TestStandardPaperSize(t *testing.T) {
	cases := []struct {
		name string
		w, h float64
		ok   bool
	}{
		{"A4", 210, 297, true},
		{"a4", 210, 297, true},
		{"B5 (JIS)", 182, 257, true},
		{"na_letter", 216, 279, true},
		{"us-legal", 216, 356, true},
		{"DL", 110, 220, true},
		{"Unknown", 0, 0, false},
		{"", 0, 0, false},
	}
	for _, tc := range cases {
		w, h, ok := standardPaperSize(tc.name)
		if ok != tc.ok || (ok && (w != tc.w || h != tc.h)) {
			t.Fatalf("standardPaperSize(%q) = (%g,%g,%v), want (%g,%g,%v)", tc.name, w, h, ok, tc.w, tc.h, tc.ok)
		}
	}
}

// TestLpTraysAndMediaTypes covers extraction and duplicate suppression.
func TestLpTraysAndMediaTypes(t *testing.T) {
	options := parseLpoptions("InputSlot/Media Source: *Auto Tray1 Tray1\nMediaType/Media Type: *Plain Glossy\n")
	trays := lpTrays(options)
	if len(trays) != 2 || trays[0].ID != "Auto" || trays[1].Name != "Tray1" {
		t.Fatalf("trays wrong: %#v", trays)
	}
	media := lpMediaTypes(options)
	if len(media) != 2 || media[0].ID != "Plain" || media[1].Name != "Glossy" {
		t.Fatalf("media types wrong: %#v", media)
	}
	if lpMediaTypes(parseLpoptions("InputSlot/Media Source: *Auto\n")) == nil {
		t.Fatal("expected non-nil empty media type slice")
	}
}

// TestParseLpstatDevices covers real `lpstat -v` forms including URIs with colons
// and a printer name containing a hyphen.
func TestParseLpstatDevices(t *testing.T) {
	out := strings.Join([]string{
		"device for Office: ipp://print.local/printers/office",
		"device for Label-Ticket: usb://HPRT/N41?serial=12345",
		"device for pdf-printer: cups-pdf:/",
		"device for broken-line-without-uri:",
		"unrelated shim line",
		"",
	}, "\n")

	devices := parseLpstatDevices(out)
	want := map[string]string{
		"Office":       "ipp://print.local/printers/office",
		"Label-Ticket": "usb://HPRT/N41?serial=12345",
		"pdf-printer":  "cups-pdf:/",
	}
	if !reflect.DeepEqual(devices, want) {
		t.Fatalf("devices = %#v, want %#v", devices, want)
	}
	if uri, ok := lookupDevice(devices, "label-ticket"); !ok || uri != want["Label-Ticket"] {
		t.Fatalf("case-insensitive lookup failed: %q %v", uri, ok)
	}
	if _, ok := lookupDevice(devices, "Missing"); ok {
		t.Fatal("missing printer must not resolve")
	}
}

// TestParseLpstatDefault covers both real `lpstat -d` shapes.
func TestParseLpstatDefault(t *testing.T) {
	cases := map[string]string{
		"system default destination: Office\n": "Office",
		"no system default destination\n":      "",
		"":                                     "",
		"system default destination:  spaced name \n": "spaced name",
	}
	for out, want := range cases {
		if got := parseLpstatDefault(out); got != want {
			t.Fatalf("parseLpstatDefault(%q) = %q, want %q", out, got, want)
		}
	}
}

// TestClassifyCUPSDevice covers local, network, virtual and unknown schemes.
func TestClassifyCUPSDevice(t *testing.T) {
	cases := []struct {
		uri     string
		local   *bool
		network *bool
		virtual *bool
	}{
		{"ipp://print.local/printers/office", boolPtr(false), boolPtr(true), boolPtr(false)},
		{"usb://HPRT/N41", boolPtr(true), boolPtr(false), boolPtr(false)},
		{"cups-pdf:/", nil, nil, boolPtr(true)},
		{"hp:/net/OfficeJet?ip=10.0.0.5", boolPtr(false), boolPtr(true), boolPtr(false)},
		{"hp:/usb/Deskjet?serial=X", boolPtr(true), boolPtr(false), boolPtr(false)},
		{"", nil, nil, nil},
		{"made-up-scheme:/x", nil, nil, nil},
	}
	for _, tc := range cases {
		local, network := classifyCUPSDevice(tc.uri)
		if !sameBoolPtr(local, tc.local) || !sameBoolPtr(network, tc.network) {
			t.Fatalf("%q: got local=%s network=%s", tc.uri, boolText(local), boolText(network))
		}
		if got := virtualPrinterFromCUPS(tc.uri); !sameBoolPtr(got, tc.virtual) {
			t.Fatalf("%q: got virtual=%s want %s", tc.uri, boolText(got), boolText(tc.virtual))
		}
	}
}

// TestUnknownPrinterOutput covers the CUPS diagnostics that mean "no such queue".
func TestUnknownPrinterOutput(t *testing.T) {
	yes := []string{
		`lpoptions: Unknown printer or class "Missing".`,
		"lpstat: No destinations added.",
		"the printer was not found",
	}
	for _, out := range yes {
		if !unknownPrinterOutput([]byte(out)) {
			t.Fatalf("expected not-found for %q", out)
		}
	}
	no := []string{
		"PageSize/Media Size: *A4",
		"",
	}
	for _, out := range no {
		if unknownPrinterOutput([]byte(out)) {
			t.Fatalf("unexpected not-found for %q", out)
		}
	}
}

// TestNewPrinterInfoInitializesLists proves the protocol invariant that papers,
// trays and media_types serialize as [] rather than null.
func TestNewPrinterInfoInitializesLists(t *testing.T) {
	info := newPrinterInfo("HPRT N41")
	if info.Name != "HPRT N41" {
		t.Fatalf("name not set: %#v", info)
	}
	if info.Papers == nil || info.Trays == nil || info.MediaTypes == nil {
		t.Fatalf("capability lists must be non-nil: %#v", info)
	}
	// Optional scalars must default to unknown, not to a fabricated zero value.
	if info.IsDefault != nil || info.DPI != nil || info.Port != nil || info.IsLocal != nil || info.IsNetwork != nil || info.IsVirtual != nil {
		t.Fatalf("optional fields must start nil: %#v", info)
	}
}

// TestPrinterInfoJSONShape pins the wire encoding the SDK consumes (snake_case
// names, empty arrays present, unknown optionals omitted).
func TestPrinterInfoJSONShape(t *testing.T) {
	info := newPrinterInfo("HPRT N41")
	info.Papers = []domain.PrinterPaper{{ID: "258", Name: "菜鸟标准 100mm * 180mm", WidthMM: 100, HeightMM: 180}}

	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(info); err != nil {
		t.Fatal(err)
	}
	text := buf.String()
	for _, want := range []string{
		`"name":"HPRT N41"`,
		`"papers":[{"id":"258","name":"菜鸟标准 100mm * 180mm","width_mm":100,"height_mm":180}]`,
		`"trays":[]`,
		`"media_types":[]`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("JSON missing %s\ngot: %s", want, text)
		}
	}
	for _, absent := range []string{"is_default", "dpi", "port", "is_local", "is_network", "is_virtual", "null"} {
		if strings.Contains(text, absent) {
			t.Fatalf("JSON must omit %q when unknown\ngot: %s", absent, text)
		}
	}
}

// TestInfoRejectsEmptyName proves an empty name is a not-found rather than a
// silent success for "whatever printer happens to be default".
func TestInfoRejectsEmptyName(t *testing.T) {
	for _, name := range []string{"", "   ", "\t"} {
		_, err := New().Info(t.Context(), name)
		if err == nil {
			t.Fatalf("Info(%q) must fail", name)
		}
		if !errors.Is(err, ErrPrinterNotFound) {
			t.Fatalf("Info(%q) must wrap ErrPrinterNotFound, got %v", name, err)
		}
	}
}

// TestInfoNonexistentPrinter proves an unknown queue maps to ErrPrinterNotFound
// through errors.Is on every platform.
func TestInfoNonexistentPrinter(t *testing.T) {
	_, err := New().Info(t.Context(), "PrintBridge Nonexistent Printer 8f3a2c")
	if err == nil {
		t.Fatal("expected an error for a nonexistent printer")
	}
	if !errors.Is(err, ErrPrinterNotFound) {
		t.Fatalf("expected ErrPrinterNotFound, got %v", err)
	}
}

// TestInfoHonoursCancelledContext proves a cancelled context short-circuits before
// any platform call, so callers can bound capability queries.
func TestInfoHonoursCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := New().Info(ctx, "HPRT N41"); err == nil {
		t.Fatal("expected an error for a cancelled context")
	}
}

// TestBackendImplementsSystemDefaulter guards the assertion the agent layer makes
// at runtime; a backend that silently lost SystemDefault would flip is_default to
// always-false without a compile error.
func TestBackendImplementsSystemDefaulter(t *testing.T) {
	var backend Backend = New()
	if _, ok := backend.(SystemDefaulter); !ok {
		t.Fatal("platform backend must implement SystemDefaulter")
	}
}

// TestRealPrinterCapabilities is the opt-in hardware test. It is skipped unless
// PRINTBRIDGE_REAL_PRINTER names a queue that actually exists on this machine.
//
//	PRINTBRIDGE_REAL_PRINTER="HPRT N41" go test ./internal/printer -run RealPrinter -v
func TestRealPrinterCapabilities(t *testing.T) {
	name := strings.TrimSpace(os.Getenv("PRINTBRIDGE_REAL_PRINTER"))
	if name == "" {
		t.Skip("set PRINTBRIDGE_REAL_PRINTER to a real queue name to run this test")
	}
	info, err := New().Info(t.Context(), name)
	if err != nil {
		t.Fatalf("Info(%q): %v", name, err)
	}
	if info.Name == "" {
		t.Fatal("expected a printer name")
	}
	if info.Papers == nil || info.Trays == nil || info.MediaTypes == nil {
		t.Fatalf("capability lists must be non-nil: %#v", info)
	}
	for _, paper := range info.Papers {
		if paper.WidthMM <= 0 || paper.HeightMM <= 0 {
			t.Fatalf("paper %#v must carry a positive size", paper)
		}
		if paper.WidthMM > 2000 || paper.HeightMM > 2000 {
			t.Fatalf("paper %#v size is out of the protocol range", paper)
		}
	}
	t.Logf("%s: papers=%d trays=%d media_types=%d dpi=%v port=%v virtual=%v local=%v network=%v default=%v",
		info.Name, len(info.Papers), len(info.Trays), len(info.MediaTypes),
		deref(info.DPI), derefString(info.Port), boolText(info.IsVirtual),
		boolText(info.IsLocal), boolText(info.IsNetwork), boolText(info.IsDefault))
	for i, paper := range info.Papers {
		if i >= 5 {
			break
		}
		t.Logf("  paper %s %q %.1fx%.1fmm", paper.ID, paper.Name, paper.WidthMM, paper.HeightMM)
	}
	for i, tray := range info.Trays {
		if i >= 5 {
			break
		}
		t.Logf("  tray %s %q", tray.ID, tray.Name)
	}
}

func derefString(v *string) any {
	if v == nil {
		return nil
	}
	return *v
}
