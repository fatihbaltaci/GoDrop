package wizard

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"math"
)

// SampleName is the picture setup leaves next to the configuration, so that
// the first command it prints is one that works.
//
// An example that says file=@photo.jpg is an example that fails: the reader
// has no photo.jpg, and the first thing they learn about GoDrop is an error
// from curl. This is the file that example points at.
const SampleName = "sample.png"

// The mark, in the proportions of site/logo.svg: a circle with a spire on top
// of it, drawn in a 256 unit box.
const (
	sampleSize = 512  // pixels, square
	dropCX     = 128  // the drop, in logo units
	dropCY     = 158  //
	dropR      = 86   //
	dropTop    = 20   // where the spire ends
	dropTaper  = 1.55 // how the sides curve in; 1 would be a plain cone
	dropStroke = 9    // the outline, as in the logo
)

// SampleImage draws that mark as a PNG.
//
// Drawn rather than embedded: an asset in the repository is one more thing to
// keep in step with the logo and one more thing a drift test has to guard,
// whereas these numbers are the ones the logo itself uses.
func SampleImage() string {
	img := image.NewNRGBA(image.Rect(0, 0, sampleSize, sampleSize))
	background := color.NRGBA{R: 0xF4, G: 0xF8, B: 0xFB, A: 0xFF}
	outline := color.NRGBA{R: 0x17, G: 0x30, B: 0x5A, A: 0xFF}

	for y := range sampleSize {
		for x := range sampleSize {
			// Four samples per pixel: the edges of a curve drawn one pixel at
			// a time look like a staircase, and this is a picture of the logo.
			var fill, edge float64
			for _, dy := range []float64{0.25, 0.75} {
				for _, dx := range []float64{0.25, 0.75} {
					lx, ly := logoUnits(float64(x)+dx), logoUnits(float64(y)+dy)
					switch {
					case insideDrop(lx, ly, dropR-dropStroke):
						fill += 0.25
					case insideDrop(lx, ly, dropR):
						edge += 0.25
					}
				}
			}
			pixel := background
			if fill > 0 {
				pixel = blend(pixel, dropColour(y), fill)
			}
			if edge > 0 {
				pixel = blend(pixel, outline, edge)
			}
			img.SetNRGBA(x, y, pixel)
		}
	}

	var buf bytes.Buffer
	// image/png cannot fail on an in-memory buffer, and there is no
	// alternative picture to fall back to if it somehow did.
	_ = png.Encode(&buf, img)
	return buf.String()
}

// logoUnits converts a pixel coordinate into the logo's 256 unit box.
func logoUnits(v float64) float64 { return v * 256 / sampleSize }

// insideDrop reports whether a point is inside a drop of the given radius:
// the circle, plus a spire whose sides curve in towards the top.
func insideDrop(x, y, r float64) bool {
	dx := math.Abs(x - dropCX)
	if y >= dropCY {
		return dx*dx+(y-dropCY)*(y-dropCY) <= r*r
	}
	top := dropTop + (dropR - r) // a thinner drop starts a little lower
	if y < top {
		return false
	}
	return dx <= r*math.Pow((y-top)/(dropCY-top), dropTaper)
}

// dropColour is the gradient down the drop, top to bottom.
func dropColour(y int) color.NRGBA {
	t := float64(y) / sampleSize
	return color.NRGBA{
		R: uint8(0x5F + (0x2C-0x5F)*t),
		G: uint8(0xD8 + (0xB4-0xD8)*t),
		B: uint8(0xEA + (0xD8-0xEA)*t),
		A: 0xFF,
	}
}

// blend paints over with alpha on top of under.
func blend(under, over color.NRGBA, alpha float64) color.NRGBA {
	mix := func(a, b uint8) uint8 { return uint8(float64(a)*(1-alpha) + float64(b)*alpha) }
	return color.NRGBA{R: mix(under.R, over.R), G: mix(under.G, over.G), B: mix(under.B, over.B), A: 0xFF}
}
