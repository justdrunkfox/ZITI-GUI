package main

import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
	"image/png"

	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/math/fixed"
)

var stateColors = map[ServiceState]color.RGBA{
	SvcActive:     {R: 0x35, G: 0xC7, B: 0x5A, A: 0xFF}, // зелёный
	SvcActivating: {R: 0xFF, G: 0xB0, B: 0x20, A: 0xFF}, // жёлтый
	SvcInactive:   {R: 0x8E, G: 0x8E, B: 0x93, A: 0xFF}, // серый
	SvcFailed:     {R: 0xFF, G: 0x45, B: 0x3A, A: 0xFF}, // красный
	SvcUnknown:    {R: 0xFF, G: 0x9F, B: 0x0A, A: 0xFF}, // оранжевый
}

// iconPNG рисует круглую иконку: тёмный диск, цветное кольцо и буква «Z».
func iconPNG(state ServiceState, ipcOK bool) []byte {
	const size = 64
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	bg := color.RGBA{R: 0x10, G: 0x1A, B: 0x33, A: 0xFF}
	cx, cy, r := float64(size)/2, float64(size)/2, float64(size)/2-2

	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			dx, dy := float64(x)-cx+0.5, float64(y)-cy+0.5
			d := sqrt(dx*dx + dy*dy)
			switch {
			case d <= r-3:
				img.Set(x, y, bg)
			case d <= r:
				img.Set(x, y, ringColor(state, ipcOK))
			}
		}
	}

	glyph := renderGlyph('Z', 28, color.RGBA{R: 0xE8, G: 0xEE, B: 0xF8, A: 0xFF})
	if glyph != nil {
		gw := glyph.Bounds().Dx()
		gh := glyph.Bounds().Dy()
		draw.Draw(img, image.Rect(size/2-gw/2, size/2-gh/2, size/2-gw/2+gw, size/2-gh/2+gh), glyph, glyph.Bounds().Min, draw.Over)
	}

	var buf bytes.Buffer
	png.Encode(&buf, img)
	return buf.Bytes()
}

func ringColor(state ServiceState, ipcOK bool) color.RGBA {
	c, ok := stateColors[state]
	if !ok {
		c = stateColors[SvcUnknown]
	}
	if state == SvcActive && !ipcOK {
		c = stateColors[SvcUnknown] // работает, но статусы недоступны
	}
	return c
}

// renderGlyph рисует букву базовым шрифтом и масштабирует до высоты height.
func renderGlyph(ch rune, height int, col color.RGBA) image.Image {
	small := image.NewRGBA(image.Rect(0, 0, 10, 14))
	d := &font.Drawer{
		Dst:  small,
		Src:  image.NewUniform(col),
		Face: basicfont.Face7x13,
		Dot:  fixed.P(1, 12),
	}
	d.DrawString(string(ch))

	// bbox нарисованного
	minX, minY, maxX, maxY := 1000, 1000, -1, -1
	for y := 0; y < 14; y++ {
		for x := 0; x < 10; x++ {
			if _, _, _, a := small.At(x, y).RGBA(); a > 0 {
				if x < minX {
					minX = x
				}
				if x > maxX {
					maxX = x
				}
				if y < minY {
					minY = y
				}
				if y > maxY {
					maxY = y
				}
			}
		}
	}
	if maxX < 0 {
		return nil
	}
	src := image.Rect(minX, minY, maxX+1, maxY+1)
	sw, sh := src.Dx(), src.Dy()
	scale := height / sh
	if scale < 1 {
		scale = 1
	}
	dst := image.NewRGBA(image.Rect(0, 0, sw*scale, sh*scale))
	for y := 0; y < dst.Bounds().Dy(); y++ {
		for x := 0; x < dst.Bounds().Dx(); x++ {
			dst.Set(x, y, small.At(src.Min.X+x/scale, src.Min.Y+y/scale))
		}
	}
	return dst
}

func sqrt(f float64) float64 {
	if f <= 0 {
		return 0
	}
	x := f
	for i := 0; i < 24; i++ {
		x = (x + f/x) / 2
	}
	return x
}
