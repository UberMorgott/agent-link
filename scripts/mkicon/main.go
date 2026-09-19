// Command mkicon draws the agentlink icon, two linked rings, at every size a
// Windows icon needs and writes them as one .ico: 16-128 px as 32-bit
// bitmaps, 256 px as PNG. Each size is drawn on its own pixel grid (integer
// radii, centres on pixel corners) rather than scaled from another size, so
// the edges stay sharp. It is run by go generate in cmd/agentlink.
//
//	go run ./scripts/mkicon cmd/agentlink/icon.ico
package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
)

// sizes are the square sizes in the icon, smallest first.
var sizes = []int{16, 20, 24, 32, 40, 48, 64, 128, 256}

var (
	blue  = color.NRGBA{R: 47, G: 111, B: 214, A: 255}
	green = color.NRGBA{R: 31, G: 157, B: 85, A: 255}
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: mkicon <out.ico>")
		os.Exit(2)
	}
	var imgs []*image.NRGBA
	for _, n := range sizes {
		imgs = append(imgs, draw(n))
	}
	data, err := encodeICO(imgs)
	if err == nil {
		err = os.WriteFile(os.Args[1], data, 0o644)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// geometry is one size's rings in pixels: the blue ring's centre is (cx, cy),
// the green one's (cx+d, cy); both have radii r (inner) and big (outer).
// Around the blue ring a gap wide pixels is cut out of the green one, so the
// blue ring lies on top.
type geometry struct {
	cx, cy, d, r, big, gap int
}

// layout scales the 32 px master (radii 6 and 10, centres 8 apart, a 1 px
// gap) to n pixels, rounding every length to whole pixels and centring the
// pair on the pixel grid.
func layout(n int) geometry {
	s := float64(n) / 32
	big := int(math.Round(10 * s))
	r := min(int(math.Round(6*s)), big-2)
	d := int(math.Round(8 * s))
	if (n-d-2*big)%2 != 0 { // keep the left edge on a whole pixel
		d++
	}
	return geometry{
		cx: (n-d-2*big)/2 + big, cy: n / 2,
		d: d, r: r, big: big, gap: max(1, int(math.Round(s))),
	}
}

// draw renders the icon at n×n with 16×16 supersampling per pixel.
func draw(n int) *image.NRGBA {
	const ss = 16
	g := layout(n)
	img := image.NewNRGBA(image.Rect(0, 0, n, n))
	for y := range n {
		for x := range n {
			var cb, cg int
			for sy := range ss {
				for sx := range ss {
					px := float64(x) + (float64(sx)+0.5)/ss
					py := float64(y) + (float64(sy)+0.5)/ss
					db := math.Hypot(px-float64(g.cx), py-float64(g.cy))
					dg := math.Hypot(px-float64(g.cx+g.d), py-float64(g.cy))
					switch {
					case db >= float64(g.r) && db <= float64(g.big):
						cb++
					case dg >= float64(g.r) && dg <= float64(g.big) &&
						(db < float64(g.r-g.gap) || db > float64(g.big+g.gap)):
						cg++
					}
				}
			}
			img.SetNRGBA(x, y, blend(cb, cg, ss*ss))
		}
	}
	return img
}

// blend mixes the covered fractions of both colours into one straight-alpha
// pixel.
func blend(cb, cg, total int) color.NRGBA {
	a := cb + cg
	if a == 0 {
		return color.NRGBA{}
	}
	mix := func(b, g uint8) uint8 { return uint8((int(b)*cb + int(g)*cg + a/2) / a) } //nolint:gosec // G115: a weighted mean of two uint8 values fits in uint8
	return color.NRGBA{
		R: mix(blue.R, green.R), G: mix(blue.G, green.G), B: mix(blue.B, green.B),
		A: uint8((255*a + total/2) / total), //nolint:gosec // G115: a <= total, so the value is at most 255
	}
}

// encodeICO writes the images as one icon file; 256 px and larger become PNG
// entries, the rest 32-bit BGRA bitmaps with an all-zero AND mask.
func encodeICO(imgs []*image.NRGBA) ([]byte, error) {
	var dir, body bytes.Buffer
	le := binary.LittleEndian
	_ = binary.Write(&dir, le, [3]uint16{0, 1, uint16(len(imgs))}) //nolint:gosec // G115: a handful of sizes
	offset := 6 + 16*len(imgs)
	for _, img := range imgs {
		n := img.Rect.Dx()
		var data []byte
		if n >= 256 {
			var b bytes.Buffer
			if err := png.Encode(&b, img); err != nil {
				return nil, err
			}
			data = b.Bytes()
		} else {
			data = bitmap(img)
		}
		entry := struct {
			W, H, Colors, Reserved uint8
			Planes, BitCount       uint16
			Size, Offset           uint32
		}{
			W: uint8(n % 256), H: uint8(n % 256), //nolint:gosec // G115: 256 is written as 0 by the format
			Planes: 1, BitCount: 32,
			Size: uint32(len(data)), Offset: uint32(offset + body.Len()), //nolint:gosec // G115: an icon file is far below 4 GiB
		}
		_ = binary.Write(&dir, le, entry)
		body.Write(data)
	}
	return append(dir.Bytes(), body.Bytes()...), nil
}

// bitmap is an icon entry as a BITMAPINFOHEADER, bottom-up BGRA rows and
// the 1-bit AND mask (all zero: the alpha channel decides).
func bitmap(img *image.NRGBA) []byte {
	n := img.Rect.Dx()
	maskRow := (n + 31) / 32 * 4
	var b bytes.Buffer
	le := binary.LittleEndian
	_ = binary.Write(&b, le, struct {
		Size                  uint32
		Width, Height         int32
		Planes, BitCount      uint16
		Compression, SizeImg  uint32
		XPels, YPels          int32
		ClrUsed, ClrImportant uint32
	}{Size: 40, Width: int32(n), Height: int32(2 * n), Planes: 1, BitCount: 32}) //nolint:gosec // G115: n <= 128
	for y := n - 1; y >= 0; y-- {
		for x := range n {
			c := img.NRGBAAt(x, y)
			b.Write([]byte{c.B, c.G, c.R, c.A})
		}
	}
	b.Write(make([]byte, maskRow*n))
	return b.Bytes()
}
