//go:build ignore

// Renders winres/icon.ico, the exe icon, from the same mark as
// assets/mark.svg: a usage ring 42% full around a dot.
//
//	go run winres/icon_gen.go
package main

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"log"
	"math"
	"os"
)

var (
	green = color.NRGBA{0x34, 0xC7, 0x59, 0xFF}
	track = color.NRGBA{0x8E, 0x8E, 0x93, 0x73} // 45% opacity
)

// Geometry of assets/mark.svg, on a 128-unit canvas.
const (
	canvas   = 128.0
	ringR    = 46.0
	ringW    = 16.0
	dotR     = 15.0
	fillFrac = 0.42
)

var sizes = []int{256, 64, 48, 40, 32, 24, 20, 16}

func main() {
	// PNG only for 256px, as Windows itself does: older readers such as
	// .NET's System.Drawing.Icon accept PNG entries only at that size.
	var images [][]byte
	for _, s := range sizes {
		img := render(s)
		if s < 256 {
			images = append(images, dib(img))
			continue
		}
		var buf bytes.Buffer
		if err := png.Encode(&buf, img); err != nil {
			log.Fatal(err)
		}
		images = append(images, buf.Bytes())
	}
	if err := os.WriteFile("winres/icon.ico", ico(images), 0o644); err != nil {
		log.Fatal(err)
	}
}

// render draws the mark at size×size with 8×8 supersampling.
func render(size int) *image.NRGBA {
	const samples = 8
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	scale := float64(size) / canvas
	c := float64(size) / 2
	in, out := (ringR-ringW/2)*scale, (ringR+ringW/2)*scale
	dot := dotR * scale
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			var r, g, b, a float64
			for sy := 0; sy < samples; sy++ {
				for sx := 0; sx < samples; sx++ {
					px := float64(x) + (float64(sx)+0.5)/samples - c
					py := float64(y) + (float64(sy)+0.5)/samples - c
					d := math.Hypot(px, py)
					var col color.NRGBA
					switch {
					case d >= in && d <= out:
						// Clockwise from twelve o'clock.
						angle := math.Mod(math.Atan2(px, -py)+2*math.Pi, 2*math.Pi) / (2 * math.Pi)
						if angle <= fillFrac {
							col = green
						} else {
							col = track
						}
					case d <= dot:
						col = green
					default:
						continue
					}
					w := float64(col.A) / 255
					r, g, b, a = r+float64(col.R)*w, g+float64(col.G)*w, b+float64(col.B)*w, a+w
				}
			}
			if a == 0 {
				continue
			}
			n := float64(samples * samples)
			img.SetNRGBA(x, y, color.NRGBA{uint8(r/a + 0.5), uint8(g/a + 0.5), uint8(b/a + 0.5), uint8(a/n*255 + 0.5)})
		}
	}
	return img
}

// dib encodes a 32-bit bottom-up bitmap with an empty AND mask, the
// classic .ico entry format.
func dib(img *image.NRGBA) []byte {
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	maskRow := (w + 31) / 32 * 4
	pixels := w * h * 4
	var buf bytes.Buffer
	le := func(v any) { _ = binary.Write(&buf, binary.LittleEndian, v) }
	le([3]uint32{40, uint32(w), uint32(h * 2)})
	le([2]uint16{1, 32})
	le([6]uint32{0, uint32(pixels + maskRow*h), 0, 0, 0, 0})
	for y := h - 1; y >= 0; y-- {
		for x := 0; x < w; x++ {
			p := img.NRGBAAt(x, y)
			buf.Write([]byte{p.B, p.G, p.R, p.A})
		}
	}
	buf.Write(make([]byte, maskRow*h))
	return buf.Bytes()
}

// ico packs encoded images into one .ico file, in the order of sizes.
func ico(images [][]byte) []byte {
	var buf bytes.Buffer
	le := func(v any) { _ = binary.Write(&buf, binary.LittleEndian, v) }
	le([3]uint16{0, 1, uint16(len(images))})
	offset := 6 + 16*len(images)
	for i, data := range images {
		dim := uint8(sizes[i] % 256) // 0 means 256
		le([4]uint8{dim, dim, 0, 0})
		le([2]uint16{1, 32})
		le([2]uint32{uint32(len(data)), uint32(offset)})
		offset += len(data)
	}
	for _, data := range images {
		buf.Write(data)
	}
	return buf.Bytes()
}
