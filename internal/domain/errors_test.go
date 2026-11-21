package domain

import (
	"errors"
	"testing"
)

// TestCodeOfWalksWrappingChain is the core guarantee of the shared error
// contract: classification must survive being wrapped by an outer layer.
func TestCodeOfWalksWrappingChain(t *testing.T) {
	inner := NewProtocolError(CodeDownloadFailed, "download blocked")
	wrapped := NewProtocolError(CodeInternalError, "job failed").WithCause(inner)
	if got := CodeOf(wrapped); got != CodeInternalError {
		t.Fatalf("CodeOf = %q, want the outermost code %q", got, CodeInternalError)
	}
	if got := CodeOf(inner); got != CodeDownloadFailed {
		t.Fatalf("CodeOf = %q", got)
	}
	if !HasCode(wrapped, CodeDownloadFailed) {
		t.Fatal("nested classification is not discoverable through HasCode")
	}
	// A plain error has no classification and defaults to INTERNAL_ERROR.
	if got := CodeOf(errors.New("boom")); got != CodeInternalError {
		t.Fatalf("CodeOf(plain) = %q", got)
	}
}

// TestMessageOfPrefersProtocolMessage keeps user-facing text stable while the
// cause stays available for logs.
func TestMessageOfPrefersProtocolMessage(t *testing.T) {
	cause := errors.New("underlying syscall failure")
	err := NewProtocolError(CodePrintFailed, "cannot reach the printer").WithCause(cause)
	if got := MessageOf(err); got != "cannot reach the printer" {
		t.Fatalf("MessageOf = %q", got)
	}
	if !errors.Is(err, cause) {
		t.Fatal("cause is not reachable via errors.Is")
	}
}

func TestProtocolErrorNilSafety(t *testing.T) {
	var e *ProtocolError
	if e.Error() != "" {
		t.Fatalf("nil Error() = %q", e.Error())
	}
	if e.WithCause(errors.New("x")) != nil {
		t.Fatal("nil WithCause must stay nil")
	}
	if CodeOf(nil) != "" {
		t.Fatalf("CodeOf(nil) = %q", CodeOf(nil))
	}
}

func TestJobValidateClassifies(t *testing.T) {
	cfg := DefaultConfig()
	cases := []struct {
		name string
		job  Job
		want string
	}{
		{"empty job id", Job{Format: "pdf"}, CodeInvalidMessage},
		{"unknown format", Job{JobID: "j", Format: "exe"}, CodeUnsupportedFormat},
		{"copies over limit", Job{JobID: "j", Format: "pdf", FileURL: "https://e/x", Copies: cfg.Limits.MaxCopies + 1}, CodeCopiesOutOfRange},
		{"negative copies", Job{JobID: "j", Format: "pdf", FileURL: "https://e/x", Copies: -1}, CodeCopiesOutOfRange},
		{"missing file_url", Job{JobID: "j", Format: "pdf"}, CodeInvalidMessage},
		{"pdf with inline html", Job{JobID: "j", Format: "pdf", FileURL: "https://e/x", HTML: "<p>"}, CodeInvalidMessage},
		{"raw with paper", Job{JobID: "j", Format: "raw", DataBase64: "QQ==", Paper: &Paper{WidthMM: 10, HeightMM: 10}}, CodeInvalidMessage},
		{"bad paper", Job{JobID: "j", Format: "pdf", FileURL: "https://e/x", Paper: &Paper{WidthMM: 0, HeightMM: 10}}, CodeInvalidMessage},
		{"wait_ms too large", Job{JobID: "j", Format: "pdf", FileURL: "https://e/x", WaitMS: 30001}, CodeInvalidMessage},
		{"raw-html without html", Job{JobID: "j", Format: "raw-html"}, CodeInvalidMessage},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.job.Validate(cfg)
			if err == nil {
				t.Fatal("expected a validation error")
			}
			if got := CodeOf(err); got != tc.want {
				t.Fatalf("code = %q, want %q (%v)", got, tc.want, err)
			}
		})
	}
}

func TestJobValidateAcceptsAllSupportedFormats(t *testing.T) {
	cfg := DefaultConfig()
	base := func(format string) Job {
		j := Job{JobID: "ok", Format: format}
		switch format {
		case "raw":
			j.DataBase64 = "QQ=="
		case "raw-html":
			j.HTML = "<main>x</main>"
		default:
			j.FileURL = "https://example.com/file"
		}
		return j
	}
	for _, format := range []string{"pdf", "image", "docx", "xlsx", "pptx", "html", "raw", "raw-html"} {
		if err := base(format).Validate(cfg); err != nil {
			t.Errorf("%s rejected: %v", format, err)
		}
	}
}
