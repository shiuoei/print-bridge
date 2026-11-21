package command

import (
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/shiuoei/print-bridge/internal/convert"
	"github.com/shiuoei/print-bridge/internal/domain"
	"github.com/shiuoei/print-bridge/internal/printer"
	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/math/fixed"
)

func (s Service) testPrint(ctx context.Context, name string, r *Response) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return domain.NewProtocolError(domain.CodeArgumentInvalid, "printer name required")
	}
	info, err := s.Printer.Info(ctx, name)
	if err != nil {
		if errors.Is(err, printer.ErrPrinterNotFound) {
			return domain.NewProtocolErrorf(domain.CodePrinterNotFound, "printer not found: %s", name)
		}
		return domain.NewProtocolError(domain.CodePrintFailed, "failed to read printer capabilities").WithCause(err)
	}
	if info == nil {
		return domain.NewProtocolError(domain.CodePrintFailed, "printer capabilities unavailable")
	}
	var paper *domain.Paper
	if configured := s.Store.Config().Printing.DefaultPaper; configured != nil {
		for _, p := range info.Papers {
			if usablePaper(p) && samePaper(*configured, p) {
				copy := *configured
				paper = &copy
				break
			}
		}
		if paper == nil {
			return domain.NewProtocolError(domain.CodePaperNotFound, "configured paper is not supported by the selected printer")
		}
	} else {
		for _, p := range info.Papers {
			if usablePaper(p) && math.Abs(p.WidthMM-210) < 1 && math.Abs(p.HeightMM-297) < 1 {
				paper = &domain.Paper{WidthMM: p.WidthMM, HeightMM: p.HeightMM}
				break
			}
		}
		if paper == nil {
			for _, p := range info.Papers {
				if usablePaper(p) {
					paper = &domain.Paper{WidthMM: p.WidthMM, HeightMM: p.HeightMM}
					break
				}
			}
		}
		if paper == nil {
			return domain.NewProtocolError(domain.CodePaperNotFound, "printer has no reported paper sizes; configure a supported paper first")
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 18*time.Second)
	defer cancel()
	dir, err := os.MkdirTemp("", "printbridge-test-*")
	if err != nil {
		return domain.NewProtocolError(domain.CodeInternalError, "cannot create test page").WithCause(err)
	}
	defer os.RemoveAll(dir)
	imagePath, pdfPath := filepath.Join(dir, "test.png"), filepath.Join(dir, "test.pdf")
	if err := makeTestPage(imagePath, *paper); err != nil {
		return domain.NewProtocolError(domain.CodeInternalError, "cannot generate test page").WithCause(err)
	}
	if err := convert.ImagePDF(imagePath, pdfPath, paper); err != nil {
		return err
	}
	submission, err := s.Printer.PrintPDF(ctx, pdfPath, name, 1, paper)
	if err != nil {
		if errors.Is(err, printer.ErrPrinterNotFound) {
			return domain.NewProtocolErrorf(domain.CodePrinterNotFound, "printer not found: %s", name)
		}
		return domain.NewProtocolError(domain.CodePrintFailed, "test page submission failed").WithCause(err)
	}
	r.Status = "submitted"
	r.Message = fmt.Sprintf("test page submitted to %s (%.1f x %.1f mm); physical output unverified", name, paper.WidthMM, paper.HeightMM)
	slog.Info("test print submitted to system queue", "printer", name, "paper_width_mm", paper.WidthMM, "paper_height_mm", paper.HeightMM, "backend", submission.Backend, "system_job_id", submission.SystemJobID)
	return nil
}

func samePaper(p domain.Paper, option domain.PrinterPaper) bool {
	return (math.Abs(p.WidthMM-option.WidthMM) <= 1 && math.Abs(p.HeightMM-option.HeightMM) <= 1) || (math.Abs(p.WidthMM-option.HeightMM) <= 1 && math.Abs(p.HeightMM-option.WidthMM) <= 1)
}

func usablePaper(p domain.PrinterPaper) bool {
	return p.Name != "" && !strings.ContainsAny(p.Name, ",\r\n") && p.WidthMM > 0 && p.HeightMM > 0
}

func makeTestPage(path string, paper domain.Paper) error {
	img := image.NewRGBA(image.Rect(0, 0, 800, 1100))
	draw.Draw(img, img.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	blue := color.RGBA{20, 87, 155, 255}
	draw.Draw(img, image.Rect(50, 55, 750, 112), image.NewUniform(blue), image.Point{}, draw.Src)
	border := color.RGBA{40, 40, 40, 255}
	for _, rect := range []image.Rectangle{image.Rect(50, 145, 750, 150), image.Rect(50, 950, 750, 955), image.Rect(50, 145, 55, 955), image.Rect(745, 145, 750, 955)} {
		draw.Draw(img, rect, image.NewUniform(border), image.Point{}, draw.Src)
	}
	d := &font.Drawer{Dst: img, Src: image.NewUniform(border), Face: basicfont.Face7x13}
	d.Dot = fixed.P(78, 205)
	d.DrawString("PRINTBRIDGE GO - TEST PAGE")
	d.Dot = fixed.P(78, 245)
	d.DrawString(fmt.Sprintf("Paper: %.1f x %.1f mm", paper.WidthMM, paper.HeightMM))
	d.Dot = fixed.P(78, 285)
	d.DrawString("One page / one copy")
	d.Dot = fixed.P(78, 325)
	d.DrawString("Submitted to the system print queue")
	d.Dot = fixed.P(78, 365)
	d.DrawString("Check physical output and paper size")
	for i, c := range []color.RGBA{{220, 65, 65, 255}, {52, 155, 92, 255}, {54, 115, 211, 255}} {
		draw.Draw(img, image.Rect(78, 420+i*110, 722, 490+i*110), image.NewUniform(c), image.Point{}, draw.Src)
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return png.Encode(f, img)
}
