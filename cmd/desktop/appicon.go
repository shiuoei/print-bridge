package main

import (
	"bytes"
	_ "embed"
	"fmt"
	"image"
	"image/color"
	"image/png"
)

// appIconPNG is the application icon, the one asset behind every place the app
// shows its mark: `mygo build` reads it from the path mygo.json names for the
// Windows icon resource, the installer's icons and the Linux desktop entry,
// while the window and the tray read this embedded copy.
//
//go:embed assets/icon.png
var appIconPNG []byte

// trayIconSize is the size mygo's tray takes as the icon's size: it builds the
// Windows notification-area icon from the PNG's own dimensions.
const trayIconSize = 32

// windowIconSize is the size the window icon is rendered at: Windows scales it
// to the title bar's and the taskbar's DPI sizes, and a modest source keeps
// that conversion cheap.
const windowIconSize = 256

// iconAt returns the application icon as a size×size PNG, area-averaging the
// embedded image so a downscale keeps its edges free of the dark fringe that
// averaging straight (non-premultiplied) colours leaves.
func iconAt(size int) ([]byte, error) {
	src, err := png.Decode(bytes.NewReader(appIconPNG))
	if err != nil {
		return nil, fmt.Errorf("app icon: %w", err)
	}
	b := src.Bounds()
	if b.Dx() == size && b.Dy() == size {
		return appIconPNG, nil
	}
	dst := image.NewNRGBA(image.Rect(0, 0, size, size))
	for y := 0; y < size; y++ {
		y0, y1 := b.Min.Y+y*b.Dy()/size, b.Min.Y+(y+1)*b.Dy()/size
		if y1 <= y0 {
			y1 = y0 + 1
		}
		for x := 0; x < size; x++ {
			x0, x1 := b.Min.X+x*b.Dx()/size, b.Min.X+(x+1)*b.Dx()/size
			if x1 <= x0 {
				x1 = x0 + 1
			}
			var r, g, bl, a uint64
			for sy := y0; sy < y1; sy++ {
				for sx := x0; sx < x1; sx++ {
					c := color.NRGBAModel.Convert(src.At(sx, sy)).(color.NRGBA)
					// Premultiply by alpha, sum, then divide by both the
					// pixel count and the summed alpha: the result's alpha is
					// the mean coverage and its colour ignores transparent
					// pixels instead of darkening toward black.
					r += uint64(c.R) * uint64(c.A)
					g += uint64(c.G) * uint64(c.A)
					bl += uint64(c.B) * uint64(c.A)
					a += uint64(c.A)
				}
			}
			if a == 0 {
				continue
			}
			dst.SetNRGBA(x, y, color.NRGBA{
				R: uint8(r/a + 1/2),
				G: uint8(g/a + 1/2),
				B: uint8(bl/a + 1/2),
				A: uint8(a/uint64((x1-x0)*(y1-y0)) + 1/2),
			})
		}
	}
	var out bytes.Buffer
	if err := png.Encode(&out, dst); err != nil {
		return nil, fmt.Errorf("app icon: %w", err)
	}
	return out.Bytes(), nil
}
