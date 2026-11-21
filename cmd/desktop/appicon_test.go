package main

import (
	"bytes"
	"image"
	"image/png"
	"testing"
)

// decodeIcon decodes a PNG the icon helpers returned.
func decodeIcon(t *testing.T, data []byte) image.Image {
	t.Helper()
	if len(data) == 0 {
		t.Fatal("no PNG data")
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode icon: %v", err)
	}
	return img
}

func TestEmbeddedIconIsSquarePNG(t *testing.T) {
	img := decodeIcon(t, appIconPNG)
	if b := img.Bounds(); b.Dx() != b.Dy() {
		t.Fatalf("embedded icon is %dx%d, want a square", b.Dx(), b.Dy())
	}
	if b := img.Bounds(); b.Dx() < 256 {
		t.Fatalf("embedded icon is %dx%d, too small for Windows' 256px icon", b.Dx(), b.Dy())
	}
}

func TestIconAt(t *testing.T) {
	for _, size := range []int{trayIconSize, windowIconSize} {
		data, err := iconAt(size)
		if err != nil {
			t.Fatalf("iconAt(%d): %v", size, err)
		}
		img := decodeIcon(t, data)
		if b := img.Bounds(); b.Dx() != size || b.Dy() != size {
			t.Errorf("iconAt(%d) is %dx%d", size, b.Dx(), b.Dy())
		}
	}
}

// The tile's rounded corners stay transparent through the downscale, and the
// printer keeps its opaque centre: a resize that lost the alpha channel would
// show a square-cornered block in the tray and the title bar.
func TestIconAtKeepsAlpha(t *testing.T) {
	img := decodeIcon(t, mustIcon(t, trayIconSize))
	if _, _, _, a := img.At(0, 0).RGBA(); a > 0x4000 { // 25% opaque
		t.Errorf("top-left corner alpha is %d, want the rounded tile's transparency", a>>8)
	}
	if _, _, _, a := img.At(trayIconSize/2, trayIconSize/2).RGBA(); a < 0xffff {
		t.Errorf("centre alpha is %d, want opaque", a>>8)
	}
}

func mustIcon(t *testing.T, size int) []byte {
	t.Helper()
	data, err := iconAt(size)
	if err != nil {
		t.Fatalf("iconAt(%d): %v", size, err)
	}
	return data
}
