package domain

import (
	"strings"
	"time"
)

type Paper struct {
	WidthMM  float64 `json:"width_mm"`
	HeightMM float64 `json:"height_mm"`
}

func (p Paper) Valid() bool {
	return p.WidthMM > 0 && p.WidthMM <= 2000 && p.HeightMM > 0 && p.HeightMM <= 2000
}

type Job struct {
	JobID       string `json:"job_id"`
	Format      string `json:"format"`
	PrinterName string `json:"printer_name,omitempty"`
	FileURL     string `json:"file_url,omitempty"`
	DataBase64  string `json:"data_base64,omitempty"`
	HTML        string `json:"html,omitempty"`
	WaitMS      int    `json:"wait_ms,omitempty"`
	Copies      int    `json:"copies,omitempty"`
	Paper       *Paper `json:"paper,omitempty"`

	// ScaleMode, Direction and Center control how a page is fitted onto the
	// requested paper. Some business front-ends send
	// them on every file job; other clients never send them. Both are
	// accepted because they only affect PDF rendering.
	//
	//   ScaleMode "actual"    print 1:1 (noscale), cropping anything past the paper
	//   ScaleMode "fill"      stretch to the paper in both dimensions
	//   ScaleMode "fit_width" fit inside the paper by width
	//   ScaleMode "fit_height" fit inside the paper by height
	//   ScaleMode "" (unset)  fit inside the paper, preserving aspect ratio
	//
	// SumatraPDF's -print-settings only knows noscale, shrink, fit and stretch,
	// so fit_width/fit_height cannot be honoured by it directly: they are mapped
	// onto the closest of those (see printer.PrintOptions). They are still
	// accepted because the front-end sets them by default, and rejecting them
	// would fail a print over an option that only refines the scaling.
	ScaleMode string `json:"scale_mode,omitempty"`
	Direction string `json:"direction,omitempty"`
	Center    *bool  `json:"center,omitempty"`
}

// Recognised scale modes. An unknown value is rejected rather than ignored so a
// caller is never left believing a crop/fit choice took effect when it did not.
const (
	ScaleActual    = "actual"
	ScaleFill      = "fill"
	ScaleFitWidth  = "fit_width"
	ScaleFitHeight = "fit_height"
)

// scaleModes is the closed set of accepted scale_mode values.
var scaleModes = map[string]bool{
	"": true, ScaleActual: true, ScaleFill: true, ScaleFitWidth: true, ScaleFitHeight: true,
}

// directionModes is the closed set of accepted direction values. Only portrait
// and landscape reach the printer; "auto" means "let the backend decide", which
// is what it does when the paper is square.
var directionModes = map[string]bool{"": true, "portrait": true, "landscape": true, "auto": true}

// PaperOptions reports the rendering choices a job was accepted with, in the
// form the print backend consumes. It resolves the unset cases so the backend
// never has to invent a default: centring is on by default because labels are
// routinely narrower than the paper, and an un-centred label sits in a corner.
func (j Job) PaperOptions() (scaleMode, direction string, center bool) {
	return j.ScaleMode, j.Direction, j.Center == nil || *j.Center
}

type Status struct {
	Type      string `json:"type"`
	RequestID string `json:"request_id,omitempty"`
	JobID     string `json:"job_id"`
	// BatchID is set for a job that belongs to a batch. It is never sent on the
	// wire (the protocol has no such field on job_status); it exists so the remote
	// reporter can correlate a status report with its batch.
	BatchID string `json:"-"`
	Status  string `json:"status"`
	Message string `json:"message"`
	// Source records where the job came from: "web_socket", "remote" or "test".
	// It is history metadata, not part of the wire status frame.
	Source string `json:"-"`
	// PrinterName, PaperName and Copies are the resolved print options, recorded
	// so history can answer "which printer, which paper, how many" for a job whose
	// configuration has since changed.
	PrinterName string `json:"-"`
	PaperName   string `json:"-"`
	Copies      int    `json:"-"`
	// ErrorCode is set on `failed` statuses so the client learns the failure
	// classification (DOWNLOAD_FAILED, FORMAT_MISMATCH, PRINT_FAILED, ...) without
	// having to parse the message. Clients ignore unknown fields, so
	// adding it is backwards compatible.
	ErrorCode string    `json:"error_code,omitempty"`
	At        time.Time `json:"at,omitempty"`
}

// MaxHTMLWaitMS bounds wait_ms for rendered HTML jobs, matching the protocol
// protocol limit.
const MaxHTMLWaitMS = 30_000

// Job sources recorded in task history.
const (
	SourceWebSocket = "web_socket"
	SourceRemote    = "remote"
	SourceTest      = "test"
)

// IsTerminalStatus reports whether a status ends a job's lifecycle.
func IsTerminalStatus(status string) bool {
	switch status {
	case "submitted", "completed", "failed", "unknown", "cancelled":
		return true
	}
	return false
}

// jobFormats is the closed set of accepted task formats. It mirrors the documented
// SDK's PrintBridgeJobType union exactly.
var jobFormats = map[string]bool{
	"pdf": true, "image": true, "raw": true,
	"docx": true, "xlsx": true, "pptx": true,
	"html": true, "raw-html": true,
}

// Validate checks one job against the configured limits and reports failures as
// typed protocol errors, so the WebSocket layer can emit the documented
// error_code instead of collapsing everything into INTERNAL_ERROR.
func (j Job) Validate(c Config) error {
	if strings.TrimSpace(j.JobID) == "" || len(j.JobID) > 200 {
		return NewProtocolError(CodeInvalidMessage, "invalid job_id")
	}
	// Copies are validated before the format switch so a raw task carrying
	// `copies` fails with the specific "raw rejects copies" message below rather
	// than a generic range error.
	if j.Copies < 0 || j.Copies > c.Limits.MaxCopies {
		return NewProtocolErrorf(CodeCopiesOutOfRange, "copies must be between 0 and %d", c.Limits.MaxCopies)
	}
	if j.Paper != nil && !j.Paper.Valid() {
		return NewProtocolError(CodeInvalidMessage, "invalid paper: width_mm and height_mm must be positive and at most 2000")
	}
	if j.WaitMS < 0 || j.WaitMS > MaxHTMLWaitMS {
		return NewProtocolErrorf(CodeInvalidMessage, "invalid wait_ms: must be between 0 and %d", MaxHTMLWaitMS)
	}
	if !jobFormats[j.Format] {
		return NewProtocolErrorf(CodeUnsupportedFormat, "unsupported format: %q", j.Format)
	}
	// wait_ms only means something for a rendered HTML page. Accepting it on
	// other formats would let a client believe a settle delay is applied when it
	// is silently ignored.
	if j.WaitMS != 0 && j.Format != "html" && j.Format != "raw-html" {
		return NewProtocolErrorf(CodeInvalidMessage, "%s does not accept wait_ms", j.Format)
	}
	if !scaleModes[j.ScaleMode] {
		return NewProtocolErrorf(CodeInvalidMessage, "invalid scale_mode: %q", j.ScaleMode)
	}
	if !directionModes[j.Direction] {
		return NewProtocolErrorf(CodeInvalidMessage, "invalid direction: %q", j.Direction)
	}
	switch j.Format {
	case "pdf", "image":
		// Both a URL and inline base64 are accepted. A business site that already
		// the bytes it already holds (label and picking-slip PDFs come back from
		// its API as base64), and a caller holding bytes should not have to host
		// them first. Exactly one source is required: silently preferring one
		// would let a caller believe the other was printed.
		if j.HTML != "" {
			return NewProtocolErrorf(CodeInvalidMessage, "%s rejects html", j.Format)
		}
		if (j.FileURL == "") == (j.DataBase64 == "") {
			return NewProtocolErrorf(CodeInvalidMessage, "%s requires exactly one of file_url or data_base64", j.Format)
		}
	case "docx", "xlsx", "pptx", "html":
		if j.FileURL == "" || j.DataBase64 != "" || j.HTML != "" {
			return NewProtocolErrorf(CodeInvalidMessage, "%s requires file_url and rejects data_base64/html", j.Format)
		}
	case "raw-html":
		if strings.TrimSpace(j.HTML) == "" || j.FileURL != "" || j.DataBase64 != "" {
			return NewProtocolError(CodeInvalidMessage, "raw-html requires non-empty html and rejects file_url/data_base64")
		}
	case "raw":
		// raw content carries its own media and copy semantics, so paper is
		// rejected: accepting it would let a caller believe the sheet size was
		// changed. `copies` is different — some clients send
		// copies on every job without meaning anything by it for raw, so the
		// field is tolerated and ignored rather than failing a print that would
		// otherwise be correct. Delivery still depends only on data_base64.
		if j.DataBase64 == "" || j.FileURL != "" || j.HTML != "" || j.Paper != nil {
			return NewProtocolError(CodeInvalidMessage, "raw rejects file_url, html and paper, and requires data_base64")
		}
	}
	return nil
}
