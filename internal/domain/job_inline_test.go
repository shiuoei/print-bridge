package domain

import "testing"

// Business front-ends send label and picking-slip PDFs as data_base64
// because its API already returns the bytes. Accepting them removes the need to
// host the file first; the receiving side is in internal/convert.
func TestPDFAndImageAcceptInlineBase64(t *testing.T) {
	c := DefaultConfig()
	for _, format := range []string{"pdf", "image"} {
		job := Job{JobID: "j-" + format, Format: format, DataBase64: b64("%PDF-1.4")}
		if err := job.Validate(c); err != nil {
			t.Fatalf("%s with data_base64 was rejected: %v", format, err)
		}
		// A URL still works.
		job = Job{JobID: "u-" + format, Format: format, FileURL: "https://example.com/x"}
		if err := job.Validate(c); err != nil {
			t.Fatalf("%s with file_url was rejected: %v", format, err)
		}
	}
}

// Exactly one source is required: silently preferring one would let a caller
// believe the other was printed.
func TestPDFRejectsBothSourcesAndNeither(t *testing.T) {
	c := DefaultConfig()
	both := Job{JobID: "j", Format: "pdf", FileURL: "https://example.com/x", DataBase64: b64("%PDF-1.4")}
	if err := both.Validate(c); err == nil {
		t.Fatal("pdf with both file_url and data_base64 was accepted")
	} else if CodeOf(err) != CodeInvalidMessage {
		t.Fatalf("code = %q, want %q", CodeOf(err), CodeInvalidMessage)
	}
	neither := Job{JobID: "j", Format: "pdf"}
	if err := neither.Validate(c); err == nil {
		t.Fatal("pdf with neither source was accepted")
	}
}

// Office conversion needs a real file path, so its formats keep requiring a URL.
func TestOfficeStillRequiresURL(t *testing.T) {
	c := DefaultConfig()
	for _, format := range []string{"docx", "xlsx", "pptx", "html"} {
		job := Job{JobID: "j", Format: format, DataBase64: b64("PK")}
		if err := job.Validate(c); err == nil {
			t.Fatalf("%s accepted data_base64", format)
		}
	}
}

// A caller that asks for a scaling choice must not have it silently ignored:
// an unknown value fails instead of falling back.
func TestScaleModeAndDirectionAreValidated(t *testing.T) {
	c := DefaultConfig()
	for _, mode := range []string{"", ScaleActual, ScaleFill, ScaleFitWidth, ScaleFitHeight} {
		job := Job{JobID: "j", Format: "raw-html", HTML: "<p>x</p>", ScaleMode: mode}
		if err := job.Validate(c); err != nil {
			t.Fatalf("scale_mode %q rejected: %v", mode, err)
		}
	}
	job := Job{JobID: "j", Format: "raw-html", HTML: "<p>x</p>", ScaleMode: "bogus"}
	if err := job.Validate(c); err == nil {
		t.Fatal("unknown scale_mode accepted")
	}
	for _, dir := range []string{"", "portrait", "landscape", "auto"} {
		job := Job{JobID: "j", Format: "raw-html", HTML: "<p>x</p>", Direction: dir}
		if err := job.Validate(c); err != nil {
			t.Fatalf("direction %q rejected: %v", dir, err)
		}
	}
	job = Job{JobID: "j", Format: "raw-html", HTML: "<p>x</p>", Direction: "sideways"}
	if err := job.Validate(c); err == nil {
		t.Fatal("unknown direction accepted")
	}
}

// Centring defaults on: business sites rely on it for labels narrower than
// the paper, and an un-centred label sits in a corner.
func TestPaperOptionsDefaultsCenterOn(t *testing.T) {
	j := Job{}
	if _, _, center := j.PaperOptions(); !center {
		t.Fatal("center must default to true")
	}
	off := false
	j.Center = &off
	if _, _, center := j.PaperOptions(); center {
		t.Fatal("explicit center=false must be honoured")
	}
	on := true
	j.Center = &on
	if _, _, center := j.PaperOptions(); !center {
		t.Fatal("explicit center=true must be honoured")
	}
}
