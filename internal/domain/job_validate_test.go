package domain

import (
	"strings"
	"testing"
)

func b64(s string) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	var out strings.Builder
	data := []byte(s)
	for i := 0; i < len(data); i += 3 {
		var chunk [3]byte
		n := copy(chunk[:], data[i:])
		out.WriteByte(alphabet[chunk[0]>>2])
		out.WriteByte(alphabet[(chunk[0]&0x03)<<4|chunk[1]>>4])
		if n > 1 {
			out.WriteByte(alphabet[(chunk[1]&0x0F)<<2|chunk[2]>>6])
		} else {
			out.WriteByte('=')
		}
		if n > 2 {
			out.WriteByte(alphabet[chunk[2]&0x3F])
		} else {
			out.WriteByte('=')
		}
	}
	return out.String()
}

// TestRawToleratesCopies records a deliberate departure from the strict rule.
//
// The original strict contract failed a raw job that carries `copies` at all. The
// business front-end sends `copies: 1` on every job it submits, raw
// included, without meaning anything by it; keeping the strict rule would fail
// every TSPL label that site prints. Raw content carries its own copy semantics,
// so the field is accepted and ignored rather than reinterpreted.
func TestRawToleratesCopies(t *testing.T) {
	c := DefaultConfig()
	job := Job{JobID: "raw-1", Format: "raw", DataBase64: b64("PRINT")}
	if err := job.Validate(c); err != nil {
		t.Fatalf("a valid raw job was rejected: %v", err)
	}
	job.Copies = 1
	if err := job.Validate(c); err != nil {
		t.Fatalf("raw with copies:1 must be tolerated, got: %v", err)
	}
	// The bound still applies: an absurd copy count is a caller mistake even
	// though raw ignores the value.
	job.Copies = c.Limits.MaxCopies + 1
	if err := job.Validate(c); err == nil {
		t.Fatal("raw with an out-of-range copies value was accepted")
	} else if CodeOf(err) != CodeCopiesOutOfRange {
		t.Fatalf("code = %q, want %q", CodeOf(err), CodeCopiesOutOfRange)
	}
}

// TestRawStillRejectsPaper keeps the part of the strict contract that still
// holds: a raw payload cannot express a paper request, so accepting one would
// let a caller believe the sheet size changed.
func TestRawStillRejectsPaper(t *testing.T) {
	c := DefaultConfig()
	job := Job{JobID: "raw-2", Format: "raw", DataBase64: b64("PRINT"), Paper: &Paper{WidthMM: 100, HeightMM: 150}}
	if err := job.Validate(c); err == nil {
		t.Fatal("raw with paper was accepted")
	}
}

// TestNonHTMLRejectsWaitMS keeps a settle delay from being silently ignored on
// formats that do not render a page.
func TestNonHTMLRejectsWaitMS(t *testing.T) {
	c := DefaultConfig()
	for _, format := range []string{"pdf", "image", "docx", "xlsx", "pptx"} {
		job := Job{JobID: "j", Format: format, FileURL: "https://example.com/x", WaitMS: 500}
		if err := job.Validate(c); err == nil {
			t.Fatalf("%s accepted wait_ms", format)
		}
	}
	// HTML formats still accept it.
	for _, format := range []string{"html", "raw-html"} {
		job := Job{JobID: "j", Format: format, WaitMS: 500}
		if format == "html" {
			job.FileURL = "https://example.com/x"
		} else {
			job.HTML = "<p>x</p>"
		}
		if err := job.Validate(c); err != nil {
			t.Fatalf("%s rejected wait_ms: %v", format, err)
		}
	}
}

// TestRawHTMLRejectsWhitespaceOnly covers the trim the validator does.
func TestRawHTMLRejectsWhitespaceOnly(t *testing.T) {
	c := DefaultConfig()
	for _, html := range []string{"", "   ", "\n\t "} {
		job := Job{JobID: "j", Format: "raw-html", HTML: html}
		if err := job.Validate(c); err == nil {
			t.Fatalf("raw-html accepted whitespace-only html %q", html)
		}
	}
	job := Job{JobID: "j", Format: "raw-html", HTML: " <p>x</p> "}
	if err := job.Validate(c); err != nil {
		t.Fatalf("valid raw-html rejected: %v", err)
	}
}

// TestWaitMSBoundIsThirtySeconds pins the protocol limit.
func TestWaitMSBoundIsThirtySeconds(t *testing.T) {
	c := DefaultConfig()
	job := Job{JobID: "j", Format: "raw-html", HTML: "<p>x</p>", WaitMS: MaxHTMLWaitMS}
	if err := job.Validate(c); err != nil {
		t.Fatalf("wait_ms at the limit was rejected: %v", err)
	}
	job.WaitMS = MaxHTMLWaitMS + 1
	if err := job.Validate(c); err == nil {
		t.Fatal("wait_ms above the limit was accepted")
	}
}
