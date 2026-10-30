//go:build ignore

// Erzeugt die PNG-Symbole aus derselben Zeichnung wie iconSVG in internal/web/pwa.go.
// Aufruf im Projektverzeichnis: go run scripts/genicons/main.go
package main

import (
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
)

const ss = 4 // Überabtastung gegen Treppenstufen

var pts = [][2]float64{{64, 272}, {160, 272}, {208, 160}, {288, 368}, {344, 240}, {448, 240}}

func inside(x, y float64) (bg, line bool) {
	// abgerundetes Quadrat 512x512, Radius 96
	cx, cy := math.Min(math.Max(x, 96), 416), math.Min(math.Max(y, 96), 416)
	bg = math.Hypot(x-cx, y-cy) <= 96
	for i := 1; i < len(pts); i++ {
		ax, ay, bx, by := pts[i-1][0], pts[i-1][1], pts[i][0], pts[i][1]
		dx, dy := bx-ax, by-ay
		t := math.Min(math.Max(((x-ax)*dx+(y-ay)*dy)/(dx*dx+dy*dy), 0), 1)
		if math.Hypot(x-(ax+t*dx), y-(ay+t*dy)) <= 20 { // Strichbreite 40, runde Enden
			return bg, true
		}
	}
	return bg, false
}

func render(n int) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, n, n))
	for py := 0; py < n; py++ {
		for px := 0; px < n; px++ {
			var r, g, b, a float64
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					x := (float64(px) + (float64(sx)+.5)/ss) * 512 / float64(n)
					y := (float64(py) + (float64(sy)+.5)/ss) * 512 / float64(n)
					bg, line := inside(x, y)
					switch {
					case line && bg:
						r, g, b, a = r+255, g+255, b+255, a+255
					case bg:
						r, g, b, a = r+0x1F, g+0x5F, b+0xBF, a+255
					}
				}
			}
			if a > 0 {
				img.SetNRGBA(px, py, color.NRGBA{uint8(r / a * 255), uint8(g / a * 255), uint8(b / a * 255), uint8(a / (ss * ss))})
			}
		}
	}
	return img
}

func main() {
	for name, n := range map[string]int{"icon-192.png": 192, "icon-512.png": 512, "apple-touch-icon.png": 180} {
		f, err := os.Create("internal/web/static/" + name)
		if err != nil {
			panic(err)
		}
		if err := png.Encode(f, render(n)); err != nil {
			panic(err)
		}
		f.Close()
	}
}
