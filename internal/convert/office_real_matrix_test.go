package convert

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/shiuoei/print-bridge/internal/domain"
)

// The fixture directory can be an ignored checkout of a synthetic OOXML
// corpus. Conversion runs against installed Office/WPS/LibreOffice but does
// not submit anything to a printer.
func TestRealOfficeFormatMatrix(t *testing.T) {
	root := os.Getenv("PRINTBRIDGE_MATRIX_OFFICE_DIR")
	if root == "" {
		t.Skip("set PRINTBRIDGE_MATRIX_OFFICE_DIR to a directory containing docx/xlsx/pptx/healthy-basic.*")
	}
	for _, format := range []string{"docx", "xlsx", "pptx"} {
		t.Run(format, func(t *testing.T) {
			source := filepath.Join(root, format, "healthy-basic."+format)
			if _, err := os.Stat(source); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 130*time.Second)
			defer cancel()
			started := time.Now()
			pdf, _, err := OfficePDF(ctx, source, format, t.TempDir())
			if err != nil {
				t.Fatalf("%s conversion: %v", format, err)
			}
			if err := VerifyPDF(pdf); err != nil {
				t.Fatalf("%s output: %v", format, err)
			}
			info, err := os.Stat(pdf)
			if err != nil {
				t.Fatal(err)
			}
			if info.Size() < 1000 {
				t.Fatalf("%s output suspiciously small: %d bytes", format, info.Size())
			}
			t.Logf("%s converted in %s, PDF %d bytes", format, time.Since(started).Round(time.Millisecond), info.Size())
		})
	}
}

// Malformed containers must fail before Office/WPS is launched. This matrix
// uses the same ignored synthetic corpus as the healthy conversion test.
func TestMalformedOfficeFormatMatrix(t *testing.T) {
	root := os.Getenv("PRINTBRIDGE_MATRIX_OFFICE_DIR")
	if root == "" {
		t.Skip("set PRINTBRIDGE_MATRIX_OFFICE_DIR to the synthetic OOXML corpus")
	}
	for _, format := range []string{"docx", "xlsx", "pptx"} {
		for _, pattern := range []string{"broken-missing-*", "truncated-*", "renamed-extension.*"} {
			matches, err := filepath.Glob(filepath.Join(root, format, pattern))
			if err != nil || len(matches) != 1 {
				t.Fatalf("%s/%s: expected one fixture, found %v: %v", format, pattern, matches, err)
			}
			t.Run(format+"/"+filepath.Base(matches[0]), func(t *testing.T) {
				_, _, err := OfficePDF(t.Context(), matches[0], format, t.TempDir())
				if code := domain.CodeOf(err); code != domain.CodeFormatMismatch {
					t.Fatalf("code = %q, want %q (err: %v)", code, domain.CodeFormatMismatch, err)
				}
			})
		}
	}
}
