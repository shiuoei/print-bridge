package convert

import (
	"archive/zip"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/shiuoei/print-bridge/internal/domain"
	"github.com/shiuoei/print-bridge/internal/security"
)

// Prepare turns a job into either a PDF file path or the raw bytes to send to
// the printer. Every failure is a *domain.ProtocolError so the agent can emit a
// stable error_code instead of a generic INTERNAL_ERROR. Errors that already
// carry a classification from a lower layer (download, image, office) are
// passed through unchanged.
//
// allowedLoopbackOrigin is the exact loopback origin this job may render from,
// or empty. It is only non-empty when the agent matched the page URL against the
// validated WebSocket Origin, so a local web app can print its own pages without
// opening loopback to every caller.
func Prepare(ctx context.Context, job domain.Job, cfg domain.Config, dir string, allowedLoopbackOrigin string) (pdf string, raw []byte, err error) {
	maxBytes := int64(cfg.Limits.MaxFileSizeMB) * 1024 * 1024
	if job.Format == "raw" {
		b, e := base64.StdEncoding.DecodeString(job.DataBase64)
		if e != nil {
			return "", nil, domain.NewProtocolError(domain.CodeInvalidMessage, "invalid raw data: data_base64 is not valid base64").WithCause(e)
		}
		if len(b) == 0 {
			return "", nil, domain.NewProtocolError(domain.CodeInvalidMessage, "invalid raw data: decoded payload is empty")
		}
		if int64(len(b)) > maxBytes {
			return "", nil, domain.NewProtocolErrorf(domain.CodeFileTooLarge, "raw payload exceeds the %d MB limit", cfg.Limits.MaxFileSizeMB)
		}
		return "", b, nil
	}
	if job.Format == "raw-html" || job.Format == "html" {
		return RenderHTML(ctx, job, cfg, dir, allowedLoopbackOrigin)
	}
	if job.Format == "pdf" && strings.HasPrefix(job.FileURL, "data:application/pdf;base64,") {
		payload := strings.TrimPrefix(job.FileURL, "data:application/pdf;base64,")
		b, e := base64.StdEncoding.DecodeString(payload)
		if e != nil {
			return "", nil, domain.NewProtocolError(domain.CodeFormatMismatch, "invalid PDF data URL: payload is not valid base64").WithCause(e)
		}
		if int64(len(b)) > maxBytes {
			return "", nil, domain.NewProtocolErrorf(domain.CodeFileTooLarge, "PDF data URL exceeds the %d MB limit", cfg.Limits.MaxFileSizeMB)
		}
		path := filepath.Join(dir, "inline.pdf")
		if err := os.WriteFile(path, b, 0600); err != nil {
			return "", nil, domain.NewProtocolErrorf(domain.CodeInternalError, "write inline PDF: %v", err).WithCause(err)
		}
		if err := VerifyPDF(path); err != nil {
			return "", nil, err
		}
		return path, nil, nil
	}
	// Inline bytes for pdf/image. A business site that already holds the label
	// and picking-slip bytes, so requiring it to host them first would add a
	// network hop and an upload path that has no other purpose.
	if job.DataBase64 != "" {
		switch job.Format {
		case "pdf", "image":
			path, err := writeInlinePayload(job, maxBytes, cfg.Limits.MaxFileSizeMB, dir)
			if err != nil {
				return "", nil, err
			}
			if job.Format == "pdf" {
				if err := VerifyPDF(path); err != nil {
					return "", nil, err
				}
				return path, nil, nil
			}
			out := filepath.Join(dir, "image.pdf")
			if err := ImagePDF(path, out, job.Paper); err != nil {
				return "", nil, err
			}
			return out, nil, nil
		}
	}
	// Reject an unknown format before touching the network: it is a client
	// mistake and must not depend on whether the URL happens to be reachable.
	switch job.Format {
	case "pdf", "image", "docx", "xlsx", "pptx":
	default:
		return "", nil, domain.NewProtocolErrorf(domain.CodeUnsupportedFormat, "unsupported format: %q", job.Format)
	}
	path, err := security.Download(ctx, job.FileURL, dir, maxBytes, time.Duration(cfg.Limits.DownloadTimeoutSeconds)*time.Second)
	if err != nil {
		// security.Download already returns a classified error; re-wrapping it
		// here would replace DOWNLOAD_FAILED/FILE_TOO_LARGE with something else.
		return "", nil, err
	}
	switch job.Format {
	case "pdf":
		if err := VerifyPDF(path); err != nil {
			return "", nil, err
		}
		return path, nil, nil
	case "image":
		out := filepath.Join(dir, "image.pdf")
		if err := ImagePDF(path, out, job.Paper); err != nil {
			return "", nil, err
		}
		return out, nil, nil
	default: // docx, xlsx, pptx
		return OfficePDF(ctx, path, job.Format, dir)
	}
}

// writeInlinePayload decodes a job's data_base64 into a file in dir and returns
// its path. The limit is checked on the decoded bytes rather than the encoded
// string, because base64 inflates its input by a third and a limit applied to the
// encoded form would admit an oversized file.
func writeInlinePayload(job domain.Job, maxBytes int64, maxMB int, dir string) (string, error) {
	b, err := base64.StdEncoding.DecodeString(job.DataBase64)
	if err != nil {
		return "", domain.NewProtocolError(domain.CodeInvalidMessage,
			"invalid data_base64: not valid base64").WithCause(err)
	}
	if len(b) == 0 {
		return "", domain.NewProtocolError(domain.CodeInvalidMessage, "invalid data_base64: decoded payload is empty")
	}
	if int64(len(b)) > maxBytes {
		return "", domain.NewProtocolErrorf(domain.CodeFileTooLarge,
			"inline payload exceeds the %d MB limit", maxMB)
	}
	name := "inline-" + job.Format
	if job.Format == "image" {
		name += ".bin"
	} else {
		name += ".pdf"
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, b, 0600); err != nil {
		return "", domain.NewProtocolErrorf(domain.CodeInternalError, "write inline payload: %v", err).WithCause(err)
	}
	return path, nil
}

// VerifyPDF rejects a file that does not start with the PDF signature. The
// downloaded bytes exist but are not the requested format, which is a
// format-mismatch rather than an internal failure.
func VerifyPDF(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return domain.NewProtocolErrorf(domain.CodeInternalError, "open PDF: %v", err).WithCause(err)
	}
	defer f.Close()
	var magic [5]byte
	if _, err := io.ReadFull(f, magic[:]); err != nil || string(magic[:]) != "%PDF-" {
		pe := domain.NewProtocolError(domain.CodeFormatMismatch, "invalid PDF: file does not start with %PDF-")
		if err != nil {
			return pe.WithCause(err)
		}
		return pe
	}
	return nil
}

func OfficePDF(ctx context.Context, source, format, dir string) (string, []byte, error) {
	if err := ctx.Err(); err != nil {
		return "", nil, err
	}
	if err := verifyOOXML(source, format); err != nil {
		return "", nil, err
	}
	input := filepath.Join(dir, "document."+format)
	if err := copyFile(source, input); err != nil {
		return "", nil, domain.NewProtocolErrorf(domain.CodeInternalError, "copy Office document: %v", err).WithCause(err)
	}
	if runtime.GOOS == "windows" {
		out, comErr := windowsOfficePDF(ctx, input, format, dir)
		if comErr == nil {
			return out, nil, nil
		}
		// A missing Office/WPS install is not a failure: it selects the
		// LibreOffice fallback. Any other COM error is a real conversion
		// failure and must be reported as such.
		if !errors.Is(comErr, ErrOfficeUnavailable) {
			return "", nil, officeFailure(comErr)
		}
	}
	tool, err := findLibreOffice()
	if err != nil {
		return "", nil, domain.NewProtocolError(domain.CodeOfficeConvertFailed, "Office conversion unavailable: neither Microsoft Office, WPS nor LibreOffice is installed").WithCause(err)
	}
	profile := filepath.Join(dir, "lo-profile")
	if err := prepareLibreOfficeProfile(profile); err != nil {
		return "", nil, domain.NewProtocolErrorf(domain.CodeOfficeConvertFailed, "prepare LibreOffice profile: %v", err).WithCause(err)
	}
	limit, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	// The hardening flags behind this invocation:
	//   --nologo        no splash/no first-run UI (headless still draws them)
	//   --nodefault     do not open a default document
	//   --nolockcheck   ignore a lock file left by another running instance
	//   --norestore     never attempt document/crash recovery on startup
	// Without --norestore a crashed earlier run makes LibreOffice open a
	// recovery dialog and the conversion times out instead of converting.
	args := []string{
		"-env:UserInstallation=file:///" + filepath.ToSlash(profile),
		"--headless", "--nologo", "--nodefault", "--nolockcheck", "--norestore",
		"--convert-to", "pdf", "--outdir", dir, input,
	}
	out, err := exec.CommandContext(limit, tool, args...).CombinedOutput()
	if err != nil {
		// The parent context being done is a cancellation, not a conversion
		// failure: the agent reports `cancelled` by matching context.Canceled,
		// so that error must survive unchanged. Only the converter's own 120s
		// budget is a genuine OFFICE_CONVERT_FAILED timeout.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", nil, ctxErr
		}
		if errors.Is(limit.Err(), context.DeadlineExceeded) {
			return "", nil, domain.NewProtocolError(domain.CodeOfficeConvertFailed, "Office conversion timed out after 120s").WithCause(err)
		}
		return "", nil, domain.NewProtocolErrorf(domain.CodeOfficeConvertFailed, "Office conversion failed: %v: %s", err, strings.TrimSpace(string(out))).WithCause(err)
	}
	pdf := filepath.Join(dir, "document.pdf")
	if err := VerifyPDF(pdf); err != nil {
		if domain.CodeOf(err) == domain.CodeFormatMismatch {
			return "", nil, domain.NewProtocolError(domain.CodeOfficeConvertFailed, "Office conversion did not produce a valid PDF").WithCause(err)
		}
		return "", nil, err
	}
	return pdf, nil, nil
}

var ErrOfficeUnavailable = errors.New("Office COM converter unavailable")

// libreOfficeMacroSecurityConfig is written into the isolated LibreOffice
// profile so every conversion runs with the highest macro security level and no
// trusted document locations.
//
// Without it, a document carrying a macro opens under LibreOffice's default
// security setting, which is lower than the level this product promises. The
// profile is per-conversion, so this cannot leak into the user's own LibreOffice
// session.
const libreOfficeMacroSecurityConfig = `<?xml version="1.0" encoding="UTF-8"?>
<oor:items xmlns:oor="http://openoffice.org/2001/registry">
  <item oor:path="/org.openoffice.Office.Common/Security/Scripting">
    <prop oor:name="MacroSecurityLevel" oor:op="fuse"><value>3</value></prop>
  </item>
</oor:items>
`

// prepareLibreOfficeProfile creates the isolated user profile and pins macro
// security to "very high" with no trusted paths.
func prepareLibreOfficeProfile(profile string) error {
	userDir := filepath.Join(profile, "user")
	if err := os.MkdirAll(userDir, 0700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(userDir, "registrymodifications.xcu"),
		[]byte(libreOfficeMacroSecurityConfig), 0600)
}

// officeFailure classifies a COM conversion failure from the Windows path. A
// converter that ran but produced no usable PDF is an OFFICE_CONVERT_FAILED;
// the input already passed the OOXML structural check, so a non-PDF output must
// not be reported as a client-side format mismatch.
func officeFailure(err error) error {
	if err == nil {
		return nil
	}
	// A cancelled job must stay matchable as a cancellation so the agent can
	// report `cancelled` instead of `failed`.
	if errors.Is(err, context.Canceled) {
		return err
	}
	if domain.CodeOf(err) == domain.CodeFormatMismatch {
		return domain.NewProtocolError(domain.CodeOfficeConvertFailed, "Office conversion did not produce a valid PDF").WithCause(err)
	}
	if classifiedOffice(err) {
		return err
	}
	return domain.NewProtocolErrorf(domain.CodeOfficeConvertFailed, "Office conversion failed: %v", err).WithCause(err)
}

// classifiedOffice reports whether err already carries a protocol code.
func classifiedOffice(err error) bool {
	var pe *domain.ProtocolError
	return errors.As(err, &pe)
}

// verifyOOXML checks that the downloaded container is a well-formed Office
// Open XML package of the requested kind. A ZIP that is not the expected
// document type is a format mismatch, not a conversion failure.
func verifyOOXML(path, format string) error {
	r, err := zip.OpenReader(path)
	if err != nil {
		return domain.NewProtocolErrorf(domain.CodeFormatMismatch, "invalid Office document: %q is not a ZIP/Open XML container", format).WithCause(err)
	}
	defer r.Close()
	wanted := map[string]string{"docx": "word/document.xml", "xlsx": "xl/workbook.xml", "pptx": "ppt/presentation.xml"}[format]
	foundTypes, foundBody := false, false
	for _, file := range r.File {
		if file.Name == "[Content_Types].xml" {
			foundTypes = true
		}
		if file.Name == wanted {
			foundBody = true
		}
	}
	if !foundTypes || !foundBody {
		return domain.NewProtocolErrorf(domain.CodeFormatMismatch, "Office file content does not match format %s", format)
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
