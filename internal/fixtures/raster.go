package fixtures

import (
	"image"
)

// lcg is a fixed 64-bit linear congruential generator. The corpus does NOT use
// math/rand: its algorithm is an implementation detail of the standard library
// (math/rand/v2 already generates a different stream), and a fixture corpus
// whose pixels depend on that is not deterministic in any useful sense. This
// generator is nine lines and is part of the pinned generator source.
type lcg struct{ s uint64 }

func newLCG(seed uint64) *lcg { return &lcg{s: seed*6364136223846793005 + 1442695040888963407} }

func (l *lcg) next() uint64 {
	l.s = l.s*6364136223846793005 + 1442695040888963407
	return l.s
}

// n returns a value in [0,max).
func (l *lcg) n(max int) int { return int((l.next() >> 33) % uint64(max)) }

// scene paints a deterministic synthetic photograph-like image: two smooth
// gradients, a few hard-edged shapes and a low-amplitude noise field. It is not
// a photograph, and it is not meant to be one — it exists so that a JPEG of it
// has realistic entropy instead of compressing to a flat colour, which would
// make the derivative budget run measure nothing.
func scene(w, h int, seed uint64) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	r := newLCG(seed)

	// Pre-draw the noise field from the generator so the pixel loop stays a
	// pure function of x, y and the noise slice.
	const noiseLen = 4096 // a power of two, so the walk below can mask
	const noiseMask = noiseLen - 1
	noise := make([]int8, noiseLen)
	for i := range noise {
		noise[i] = int8(r.n(17) - 8)
	}

	// A handful of blocks with hard edges, so the DCT has something to do.
	type block struct{ x0, y0, x1, y1, cr, cg, cb int }
	blocks := make([]block, 0, 24)
	for i := 0; i < 24; i++ {
		x0 := r.n(w)
		y0 := r.n(h)
		bw := 1 + r.n(w/6+1)
		bh := 1 + r.n(h/6+1)
		x1, y1 := x0+bw, y0+bh
		if x1 > w {
			x1 = w
		}
		if y1 > h {
			y1 = h
		}
		blocks = append(blocks, block{
			x0: x0, y0: y0, x1: x1, y1: y1,
			cr: r.n(256), cg: r.n(256), cb: r.n(256),
		})
	}

	// Three passes rather than one, purely for speed: testing every block at
	// every pixel is O(pixels x blocks), which at 12 megapixels is 288 million
	// inner iterations and makes `make ci` minutes slower for no benefit. The
	// arithmetic is unchanged — the gradient, then each block averaged in over
	// the pixels it covers IN ORDER, then the noise — so the bytes are the same
	// as the naive loop's.
	// uint8 rather than int32: every value here is a colour component in
	// [0,255] both before and after the block averaging, and a 12-megapixel
	// int32 buffer is 144 MB of instrumented memory for no gain.
	base := make([]uint8, w*h*3)
	for y := 0; y < h; y++ {
		cg := uint8(30 + (180*y)/h)
		row := y * w * 3
		for x := 0; x < w; x++ {
			i := row + x*3
			base[i] = uint8(40 + (200*x)/w)
			base[i+1] = cg
			base[i+2] = uint8(60 + (150 * (x + y) / (w + h)))
		}
	}
	for _, b := range blocks {
		br, bg, bb := b.cr, b.cg, b.cb
		for y := b.y0; y < b.y1; y++ {
			row := y * w * 3
			for x := b.x0; x < b.x1; x++ {
				i := row + x*3
				base[i] = uint8((int(base[i]) + br) / 2)
				base[i+1] = uint8((int(base[i+1]) + bg) / 2)
				base[i+2] = uint8((int(base[i+2]) + bb) / 2)
			}
		}
	}
	// len(noise) is a power of two, so the index walks with a mask instead of a
	// modulo, and the pixels are written straight into Pix. Same arithmetic,
	// same bytes; this loop runs 12 million times for the budget fixture and
	// the race detector multiplies every one of those.
	for y := 0; y < h; y++ {
		row := y * w * 3
		pix := y * img.Stride
		ni := (y * 31) & noiseMask
		for x := 0; x < w; x++ {
			i := row + x*3
			nz := int(noise[ni])
			ni = (ni + 17) & noiseMask
			p := pix + x*4
			img.Pix[p] = clamp8(int(base[i]) + nz)
			img.Pix[p+1] = clamp8(int(base[i+1]) + nz)
			img.Pix[p+2] = clamp8(int(base[i+2]) - nz)
			img.Pix[p+3] = 255
		}
	}
	return img
}

func clamp8(v int) uint8 {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return uint8(v)
}
