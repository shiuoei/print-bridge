//go:build !windows

package printer

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/shiuoei/print-bridge/internal/domain"
)

// maxCommandOutput bounds the stdout this package buffers from CUPS tools. A
// printer with a thousand PPD options is still far below this.
const maxCommandOutput = 1 << 20

// lpOutput runs one CUPS command and returns stdout and stderr together.
//
// The output is capped so a wedged or chatty backend cannot exhaust memory, and
// the call is context-bound so cancellation stops it. commandError is deliberately
// not used: an error string is returned to the caller only after the partial
// output has been parsed, because lpoptions can fail and still print usable data.
func lpOutput(ctx context.Context, tool string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, tool, args...)
	// LC_ALL=C keeps the diagnostics parseable; unknownPrinterOutput matches
	// English markers and would miss a localized "Unbekannter Drucker".
	cmd.Env = append(os.Environ(), "LC_ALL=C", "LANG=C")
	var stdout, stderr boundedBuffer
	stdout.limit = maxCommandOutput
	stderr.limit = maxCommandOutput
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return append(stdout.Bytes(), stderr.Bytes()...), err
}

// boundedBuffer is an io.Writer that keeps at most limit bytes and silently
// discards the rest, so a runaway command cannot grow the heap without bound.
type boundedBuffer struct {
	data  []byte
	limit int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	original := len(p)
	if b.limit > 0 {
		if remaining := b.limit - len(b.data); remaining <= 0 {
			return original, nil
		} else if len(p) > remaining {
			p = p[:remaining]
		}
	}
	b.data = append(b.data, p...)
	return original, nil
}

func (b *boundedBuffer) Bytes() []byte { return b.data }

// Info reports the capabilities of one CUPS printer.
//
// Everything optional is best-effort: when lpstat or lpoptions is missing, not
// running, or unable to describe the queue, the name is still returned. A paper
// list is never synthesized from guesswork - the dimensions are the whole point
// of the field, so an unprovable size is omitted.
func (platformBackend) Info(ctx context.Context, name string) (*domain.PrinterInfo, error) {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return nil, fmt.Errorf("printer name required: %w", ErrPrinterNotFound)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	info := newPrinterInfo(trimmed)

	// lpstat -v gives the device URI; it is also the cheapest existence check.
	devicesOut, devicesErr := lpOutput(ctx, "lpstat", "-v")
	devices := parseLpstatDevices(string(devicesOut))
	uri, known := lookupDevice(devices, trimmed)
	if known {
		info.Port = stringPtr(uri)
		info.IsLocal, info.IsNetwork = classifyCUPSDevice(uri)
		info.IsVirtual = virtualPrinterFromCUPS(uri)
	}

	// `lpoptions -p <printer> -l` fails for an unknown queue, and it is the only
	// source that proves the queue exists when lpstat is unavailable.
	optionsOut, optionsErr := lpOutput(ctx, "lpoptions", "-p", trimmed, "-l")
	if optionsErr != nil && !known && unknownPrinterOutput(optionsOut) {
		return nil, fmt.Errorf("%q: %w", trimmed, ErrPrinterNotFound)
	}
	if !known && optionsErr != nil {
		// Neither tool could describe the queue. Report not-found rather than
		// emitting a record that claims a nonexistent device exists.
		if devicesErr != nil {
			return nil, fmt.Errorf("%q: %w", trimmed, ErrPrinterNotFound)
		}
		return nil, fmt.Errorf("%q: %w", trimmed, ErrPrinterNotFound)
	}

	options := parseLpoptions(string(optionsOut))
	info.Papers = lpPapers(options)
	info.Trays = lpTrays(options)
	info.MediaTypes = lpMediaTypes(options)
	if dpi := lpDPI(options); dpi != nil {
		info.DPI = dpi
	}

	// An explicit is_default marker in lpoptions is authoritative; the system
	// default from `lpstat -d` is the fallback.
	if def := lpDefaultChoice(options); def != nil {
		info.IsDefault = def
	} else if systemDefault, err := (platformBackend{}).SystemDefault(ctx); err == nil && systemDefault != "" {
		info.IsDefault = boolPtr(strings.EqualFold(systemDefault, trimmed))
	}
	return info, nil
}

// lpDPI reads the Resolution option when the driver publishes one.
func lpDPI(options []lpOption) *int {
	for _, choice := range lpOptionValues(options, "Resolution") {
		if dpi := parseResolutionChoice(choice.Value); dpi != nil {
			return dpi
		}
	}
	return nil
}

// parseResolutionChoice parses CUPS resolution spellings such as "600dpi" and
// "600x600dpi".
func parseResolutionChoice(value string) *int {
	text := strings.ToLower(strings.TrimSpace(value))
	text = strings.TrimSuffix(text, "dpi")
	if x := strings.IndexByte(text, 'x'); x > 0 {
		text = text[:x]
	}
	dpi, err := parsePositiveInt(text)
	if err != nil || dpi <= 0 || dpi > 100000 {
		return nil
	}
	return intPtr(dpi)
}

// parsePositiveInt is a small helper so the resolution parser can reject
// trailing garbage without importing strconv at the call site.
func parsePositiveInt(text string) (int, error) {
	value := 0
	if text == "" {
		return 0, fmt.Errorf("empty")
	}
	for _, ch := range text {
		if ch < '0' || ch > '9' {
			return 0, fmt.Errorf("not a number: %q", text)
		}
		value = value*10 + int(ch-'0')
		if value > 1000000 {
			return 0, fmt.Errorf("out of range: %q", text)
		}
	}
	return value, nil
}

// lpDefaultChoice reports whether lpoptions marked a default. CUPS only prints
// the '*' marker for the queue's own current default, so a marked choice proves
// this queue is the default destination.
func lpDefaultChoice(options []lpOption) *bool {
	for _, option := range options {
		for _, choice := range option.Choices {
			if choice.Default {
				return boolPtr(true)
			}
		}
	}
	return nil
}

// SystemDefault returns the CUPS system default destination.
func (platformBackend) SystemDefault(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	out, err := lpOutput(ctx, "lpstat", "-d")
	name := parseLpstatDefault(string(out))
	if name != "" {
		return name, nil
	}
	if err != nil {
		// "no system default destination" exits non-zero; that is a valid empty
		// answer, not a failure to report upward.
		if strings.Contains(strings.ToLower(string(out)), "no system default") {
			return "", nil
		}
		return "", commandError(out, err)
	}
	return "", nil
}
