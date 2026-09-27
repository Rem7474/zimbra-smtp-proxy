// Package icon dessine l'icône de l'application (enveloppe sur pastille de
// couleur) et l'encode au format .ico, sans fichier image à embarquer.
package icon

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"math"
)

var (
	Blue   = color.RGBA{0x1f, 0x6f, 0xeb, 0xff} // icône de l'exe / installeur
	Green  = color.RGBA{0x1a, 0x7f, 0x37, 0xff} // passerelle active, Zimbra OK
	Orange = color.RGBA{0xd1, 0x7a, 0x00, 0xff} // passerelle active, Zimbra injoignable
	Grey   = color.RGBA{0x6e, 0x77, 0x81, 0xff} // passerelle arrêtée
)

// Draw rend l'icône en size×size avec un anticrénelage 4×4.
func Draw(size int, bg color.RGBA) *image.RGBA {
	const ss = 4
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	// Trait d'au moins ~1,1 px pour rester lisible en 16×16.
	h := math.Max(0.03, 0.6/float64(size))
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			var r, g, b, a float64
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					px := (float64(x) + (float64(sx)+0.5)/ss) / float64(size)
					py := (float64(y) + (float64(sy)+0.5)/ss) / float64(size)
					c, ok := shade(px, py, h, bg)
					if ok {
						r, g, b, a = r+float64(c.R), g+float64(c.G), b+float64(c.B), a+255
					}
				}
			}
			n := float64(ss * ss)
			// image.RGBA est en alpha prémultiplié : les échantillons vides comptent pour 0.
			img.SetRGBA(x, y, color.RGBA{uint8(r / n), uint8(g / n), uint8(b / n), uint8(a / n)})
		}
	}
	return img
}

func shade(x, y, h float64, bg color.RGBA) (color.RGBA, bool) {
	if math.Hypot(x-0.5, y-0.5) > 0.48 {
		return color.RGBA{}, false
	}
	const x0, y0, x1, y1, vy = 0.25, 0.32, 0.75, 0.68, 0.53
	inOuter := x >= x0-h && x <= x1+h && y >= y0-h && y <= y1+h
	inInner := x > x0+h && x < x1-h && y > y0+h && y < y1-h
	onV := segDist(x, y, x0, y0, 0.5, vy) < h || segDist(x, y, x1, y0, 0.5, vy) < h
	if (inOuter && !inInner) || onV {
		return color.RGBA{0xff, 0xff, 0xff, 0xff}, true
	}
	return bg, true
}

func segDist(px, py, ax, ay, bx, by float64) float64 {
	dx, dy := bx-ax, by-ay
	t := math.Max(0, math.Min(1, ((px-ax)*dx+(py-ay)*dy)/(dx*dx+dy*dy)))
	return math.Hypot(px-(ax+t*dx), py-(ay+t*dy))
}

// ICO dessine l'icône aux tailles demandées et les regroupe dans un .ico.
func ICO(bg color.RGBA, sizes ...int) []byte {
	le := binary.LittleEndian
	var hdr, data bytes.Buffer
	binary.Write(&hdr, le, [3]uint16{0, 1, uint16(len(sizes))})
	offset := 6 + 16*len(sizes)
	for _, s := range sizes {
		d := dib(Draw(s, bg))
		dim := byte(s)
		if s >= 256 {
			dim = 0
		}
		hdr.Write([]byte{dim, dim, 0, 0})
		binary.Write(&hdr, le, [2]uint16{1, 32})
		binary.Write(&hdr, le, [2]uint32{uint32(len(d)), uint32(offset)})
		offset += len(d)
		data.Write(d)
	}
	return append(hdr.Bytes(), data.Bytes()...)
}

// dib encode une image en DIB 32 bits BGRA (bas en haut) + masque AND vide.
func dib(img *image.RGBA) []byte {
	s := img.Bounds().Dx()
	le := binary.LittleEndian
	var buf bytes.Buffer
	binary.Write(&buf, le, struct {
		Size          uint32
		W, H          int32
		Planes, Bits  uint16
		Compr, ImgLen uint32
		XPPM, YPPM    int32
		Used, Imp     uint32
	}{Size: 40, W: int32(s), H: int32(2 * s), Planes: 1, Bits: 32})
	for y := s - 1; y >= 0; y-- {
		for x := 0; x < s; x++ {
			c := color.NRGBAModel.Convert(img.RGBAAt(x, y)).(color.NRGBA)
			buf.Write([]byte{c.B, c.G, c.R, c.A})
		}
	}
	buf.Write(make([]byte, ((s+31)/32)*4*s))
	return buf.Bytes()
}
