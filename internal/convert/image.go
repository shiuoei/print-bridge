package convert

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	_ "image/png"
	"math"
	"os"

	"github.com/shiuoei/print-bridge/internal/domain"
)

// maxImagePixels bounds the decoded raster so a decompression bomb cannot
// exhaust memory while building the PDF page.
const maxImagePixels = 100_000_000

// labelDPI is the nominal resolution assumed when an image has to be given a
// physical size without a page to fit into. Label printers in this product's
// target fleet run at 203 dpi, which is also the assumption the original
// implementation documented.
const labelDPI = 203.0

// fitRect is an axis-aligned placement in PDF points.
type fitRect struct {
	x, y, width, height float64
}

// fitContain returns the largest rectangle with the image's aspect ratio that
// fits inside the page, centred on it.
//
// Stretching an image to fill the page is wrong for every image whose aspect
// ratio differs from the paper: a 4:3 photo on a 100x180 mm label comes out
// visibly skewed. The original implementation used the same contain-and-centre
// rule, so keeping it here also keeps print output stable across the port.
func fitContain(imageWidth, imageHeight, pageWidth, pageHeight float64) (fitRect, bool) {
	if !isPositiveFinite(imageWidth) || !isPositiveFinite(imageHeight) ||
		!isPositiveFinite(pageWidth) || !isPositiveFinite(pageHeight) {
		return fitRect{}, false
	}
	scale := math.Min(pageWidth/imageWidth, pageHeight/imageHeight)
	width := imageWidth * scale
	height := imageHeight * scale
	return fitRect{
		x:      (pageWidth - width) / 2,
		y:      (pageHeight - height) / 2,
		width:  width,
		height: height,
	}, true
}

// isPositiveFinite rejects NaN, ±Inf and non-positive values.
func isPositiveFinite(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0) && v > 0
}

// mmToPt converts millimetres to PDF points (1/72 inch).
func mmToPt(mm float64) float64 { return mm * 72 / 25.4 }

// validateImageBounds rejects a raster whose decoded dimensions cannot produce
// a printable page. Both the empty case and the pixel cap are the same class of
// failure: the downloaded bytes are not usable as an image.
func validateImageBounds(width, height int) error {
	if width < 1 || height < 1 {
		return domain.NewProtocolErrorf(domain.CodeFormatMismatch, "invalid image dimensions %dx%d: width and height must be at least 1 pixel", width, height)
	}
	if int64(width)*int64(height) > maxImagePixels {
		return domain.NewProtocolErrorf(domain.CodeFormatMismatch, "invalid image dimensions %dx%d: at most %d pixels are allowed", width, height, maxImagePixels)
	}
	return nil
}

func ImagePDF(source, dest string, paper *domain.Paper) error {
	f, err := os.Open(source)
	if err != nil {
		return domain.NewProtocolErrorf(domain.CodeInternalError, "open image: %v", err).WithCause(err)
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	if err != nil {
		return domain.NewProtocolErrorf(domain.CodeFormatMismatch, "invalid image: %v", err).WithCause(err)
	}
	b := img.Bounds()
	if err := validateImageBounds(b.Dx(), b.Dy()); err != nil {
		return err
	}
	white := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(white, white.Bounds(), &image.Uniform{C: color.White}, image.Point{}, draw.Src)
	draw.Draw(white, white.Bounds(), img, b.Min, draw.Over)
	var jpegData bytes.Buffer
	if err := jpeg.Encode(&jpegData, white, &jpeg.Options{Quality: 95}); err != nil {
		return domain.NewProtocolErrorf(domain.CodeInternalError, "encode image: %v", err).WithCause(err)
	}
	// Natural size: what the image would occupy if rendered at the label DPI
	// assumption. When a page is given the image is fitted into it; without a
	// page the natural size defines the page, which keeps a bare image job
	// printing at its intended physical size instead of an arbitrary 96 dpi.
	naturalWidthMM := float64(b.Dx()) * 25.4 / labelDPI
	naturalHeightMM := float64(b.Dy()) * 25.4 / labelDPI

	pageWidthMM, pageHeightMM := naturalWidthMM, naturalHeightMM
	if paper != nil {
		pageWidthMM, pageHeightMM = paper.WidthMM, paper.HeightMM
	}
	fit, ok := fitContain(naturalWidthMM, naturalHeightMM, pageWidthMM, pageHeightMM)
	if !ok {
		return domain.NewProtocolErrorf(domain.CodeFormatMismatch,
			"invalid image placement for a %.1fx%.1f mm page", pageWidthMM, pageHeightMM)
	}
	w, h := mmToPt(pageWidthMM), mmToPt(pageHeightMM)
	// The image XObject is unit-sized (see the /Matrix-free image object below),
	// so the placement transform carries the final width and height directly.
	content := []byte(fmt.Sprintf("q %.3f 0 0 %.3f %.3f %.3f cm /Im0 Do Q\n",
		mmToPt(fit.width), mmToPt(fit.height), mmToPt(fit.x), mmToPt(fit.y)))
	var pdf bytes.Buffer
	pdf.WriteString("%PDF-1.4\n")
	offsets := []int{0}
	object := func(id int, data []byte) {
		offsets = append(offsets, pdf.Len())
		fmt.Fprintf(&pdf, "%d 0 obj\n", id)
		pdf.Write(data)
		pdf.WriteString("\nendobj\n")
	}
	object(1, []byte("<< /Type /Catalog /Pages 2 0 R >>"))
	object(2, []byte("<< /Type /Pages /Kids [3 0 R] /Count 1 >>"))
	object(3, []byte(fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 %.3f %.3f] /Resources << /XObject << /Im0 5 0 R >> >> /Contents 4 0 R >>", w, h)))
	object(4, append([]byte(fmt.Sprintf("<< /Length %d >>\nstream\n", len(content))), append(content, []byte("endstream")...)...))
	imageObject := []byte(fmt.Sprintf("<< /Type /XObject /Subtype /Image /Width %d /Height %d /ColorSpace /DeviceRGB /BitsPerComponent 8 /Filter /DCTDecode /Length %d >>\nstream\n", b.Dx(), b.Dy(), jpegData.Len()))
	imageObject = append(imageObject, jpegData.Bytes()...)
	imageObject = append(imageObject, []byte("\nendstream")...)
	object(5, imageObject)
	xref := pdf.Len()
	fmt.Fprintf(&pdf, "xref\n0 %d\n0000000000 65535 f \n", len(offsets))
	for _, offset := range offsets[1:] {
		fmt.Fprintf(&pdf, "%010d 00000 n \n", offset)
	}
	fmt.Fprintf(&pdf, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offsets), xref)
	// A failed write is not a formatting problem: the image decoded fine and
	// the converter ran, so this is an internal failure.
	writeErr := os.WriteFile(dest, pdf.Bytes(), 0600)
	if writeErr != nil {
		return domain.NewProtocolErrorf(domain.CodeInternalError, "write image PDF: %v", writeErr).WithCause(writeErr)
	}
	return nil
}
