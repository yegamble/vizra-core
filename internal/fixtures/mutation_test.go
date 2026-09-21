package fixtures

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/png"
	"testing"
)

// Every fixture's property assertions are only worth something if they can
// fail. A flipped byte in the middle of a JPEG scan is NOT the right test for
// that: the file still is a JPEG with EXIF orientation 6 and GPS, and the
// manifest's sha256 is what catches random corruption.
//
// What has to be demonstrated is that each fixture's assertions constrain the
// thing the fixture EXISTS FOR. So each case below removes exactly that
// property and nothing else, and the assertions must reject it.
func TestRemovingThePropertyAFixtureExistsForIsCaught(t *testing.T) {
	cases := []struct {
		fixture string
		name    string
		mutate  func(t *testing.T, b []byte) []byte
	}{
		{
			"jpeg-exif-orientation6-gps", "orientation set to 1 (no rotation)",
			func(t *testing.T, b []byte) []byte {
				// tag 0x0112, type SHORT, count 1, value 6 -> value 1
				return replaceOnce(t, b,
					[]byte{0x01, 0x12, 0x00, 0x03, 0x00, 0x00, 0x00, 0x01, 0x00, 0x06},
					[]byte{0x01, 0x12, 0x00, 0x03, 0x00, 0x00, 0x00, 0x01, 0x00, 0x01})
			},
		},
		{
			"jpeg-exif-orientation6-gps", "GPS IFD pointer renamed away",
			func(t *testing.T, b []byte) []byte {
				// 0x8825 (GPSInfoIFDPointer) -> 0x9000, which keeps the IFD's
				// ascending tag order valid and removes only the GPS link.
				return replaceOnce(t, b,
					[]byte{0x88, 0x25, 0x00, 0x04, 0x00, 0x00, 0x00, 0x01},
					[]byte{0x90, 0x00, 0x00, 0x04, 0x00, 0x00, 0x00, 0x01})
			},
		},
		{
			"png-alpha", "alpha channel dropped",
			func(t *testing.T, b []byte) []byte {
				src, err := png.Decode(bytes.NewReader(b))
				if err != nil {
					t.Fatal(err)
				}
				out := image.NewRGBA(src.Bounds())
				for y := src.Bounds().Min.Y; y < src.Bounds().Max.Y; y++ {
					for x := src.Bounds().Min.X; x < src.Bounds().Max.X; x++ {
						r, g, bb, _ := src.At(x, y).RGBA()
						out.SetRGBA(x, y, color.RGBA{R: uint8(r >> 8), G: uint8(g >> 8), B: uint8(bb >> 8), A: 255})
					}
				}
				var buf bytes.Buffer
				if err := png.Encode(&buf, out); err != nil {
					t.Fatal(err)
				}
				return buf.Bytes()
			},
		},
		{
			"jpeg-truncated", "the stream completed with an EOI",
			func(t *testing.T, b []byte) []byte {
				return append(append([]byte(nil), b...), 0xFF, 0xD9)
			},
		},
		{
			"png-oversized-dimensions", "dimensions reduced to something ordinary",
			func(t *testing.T, b []byte) []byte {
				return rewriteIHDRDims(t, b, 100, 100)
			},
		},
		{
			"png-decoder-bomb", "IDAT filled with incompressible data",
			func(t *testing.T, b []byte) []byte {
				// Same declared dimensions, but a raster that does not inflate:
				// the expansion ratio is what this fixture is for.
				r := newLCG(42)
				out, err := rawPNG(bombDim, 8, 8, 6, bombDim*4, bigRasterLevel, func(y int, dst []byte) {
					for i := range dst {
						dst[i] = byte(r.n(256))
					}
				})
				if err != nil {
					t.Fatal(err)
				}
				return rewriteIHDRDims(t, out, bombDim, bombDim)
			},
		},
		{
			"polyglot-gif-svg-active", "the GIF prefix removed, so nothing sniffs wrong",
			func(t *testing.T, b []byte) []byte {
				i := bytes.Index(b, []byte("<?xml"))
				if i < 0 {
					t.Fatal("no XML declaration to cut back to")
				}
				return append([]byte(nil), b[i:]...)
			},
		},
		{
			"polyglot-gif-svg-active", "the active content removed",
			func(t *testing.T, b []byte) []byte {
				return replaceOnce(t, b, []byte("<script"), []byte("<desc  "))
			},
		},
		{
			"gif-animated", "reduced to a single frame",
			func(t *testing.T, b []byte) []byte {
				g, err := gif.DecodeAll(bytes.NewReader(b))
				if err != nil {
					t.Fatal(err)
				}
				var buf bytes.Buffer
				one := &gif.GIF{
					Image:     g.Image[:1],
					Delay:     g.Delay[:1],
					Disposal:  g.Disposal[:1],
					LoopCount: g.LoopCount,
				}
				if err := gif.EncodeAll(&buf, one); err != nil {
					t.Fatal(err)
				}
				return buf.Bytes()
			},
		},
		{
			"webp-animated", "the VP8X ANIMATION flag cleared",
			func(t *testing.T, b []byte) []byte {
				out := append([]byte(nil), b...)
				// RIFF(4) size(4) WEBP(4) VP8X(4) size(4) -> flags byte
				if string(out[12:16]) != "VP8X" {
					t.Fatalf("expected a VP8X chunk at offset 12, found %q", string(out[12:16]))
				}
				out[20] &^= vp8xAnimation
				return out
			},
		},
		{
			"avif-still", "the ftyp brand changed away from avif",
			func(t *testing.T, b []byte) []byte {
				return replaceOnce(t, b, []byte("ftypavif"), []byte("ftypmif1"))
			},
		},
		{
			"jpeg-12mp-budget", "the same dimensions filled with a flat colour",
			func(t *testing.T, b []byte) []byte {
				img := image.NewRGBA(image.Rect(0, 0, budgetW, budgetH))
				for i := range img.Pix {
					img.Pix[i] = 0x80
				}
				out, err := encodeJPEG(img, 85)
				if err != nil {
					t.Fatal(err)
				}
				return out
			},
		},
		{
			"mp4-short", "the track handler changed from video to audio",
			func(t *testing.T, b []byte) []byte {
				return replaceOnce(t, b, []byte("hdlr\x00\x00\x00\x00\x00\x00\x00\x00vide"),
					[]byte("hdlr\x00\x00\x00\x00\x00\x00\x00\x00soun"))
			},
		},
		{
			"webm-short", "the EBML DocType changed away from webm",
			func(t *testing.T, b []byte) []byte {
				return replaceOnce(t, b, []byte("webm"), []byte("mkvx"))
			},
		},
		{
			"webm-short", "the reproducibility normalisation of the TrackUID undone",
			func(t *testing.T, b []byte) []byte {
				if n := bytes.Count(b, codecTrackUID); n != 2 {
					t.Fatalf("the normalised TrackUID occurs %d times, want 2", n)
				}
				return bytes.ReplaceAll(append([]byte(nil), b...), codecTrackUID, []byte{1, 2, 3, 4, 5, 6, 7, 8})
			},
		},
	}

	dir, _ := sharedCorpus(t)
	specs := map[string]Spec{}
	for _, s := range Corpus() {
		specs[s.ID] = s
	}
	covered := map[string]bool{}

	for _, tc := range cases {
		t.Run(tc.fixture+"/"+tc.name, func(t *testing.T) {
			s, ok := specs[tc.fixture]
			if !ok {
				t.Fatalf("no fixture %q", tc.fixture)
			}
			covered[tc.fixture] = true
			b := readFixture(t, dir, s.Path)
			// Green first: the unmutated fixture must pass, or the case below
			// proves nothing.
			if err := s.verifyFn(b); err != nil {
				t.Fatalf("the unmutated fixture already fails: %v", err)
			}
			mutated := tc.mutate(t, b)
			if bytes.Equal(mutated, b) {
				t.Fatal("the mutation did not change the bytes")
			}
			if err := s.verifyFn(mutated); err == nil {
				t.Errorf("%s survived %q: the assertions do not constrain the property this fixture exists for", s.Path, tc.name)
			} else {
				t.Logf("rejected as expected: %v", err)
			}
		})
	}

	// Every fixture must have at least one mutation case, or a fixture could be
	// added with assertions nobody ever showed could fail.
	for _, s := range Corpus() {
		if !covered[s.ID] {
			t.Errorf("%s has no mutation case; its property assertions have never been shown to fail", s.ID)
		}
	}
}

func readFixture(t *testing.T, dir, name string) []byte {
	t.Helper()
	b, err := readFile(dir, name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func replaceOnce(t *testing.T, b, old, new []byte) []byte {
	t.Helper()
	if n := bytes.Count(b, old); n != 1 {
		t.Fatalf("the pattern to mutate occurs %d times, want exactly 1", n)
	}
	return bytes.Replace(append([]byte(nil), b...), old, new, 1)
}

// rewriteIHDRDims rewrites the declared dimensions and fixes the chunk CRC, so
// the mutation changes only what it means to change.
func rewriteIHDRDims(t *testing.T, b []byte, w, h int) []byte {
	t.Helper()
	out := append([]byte(nil), b...)
	if string(out[12:16]) != "IHDR" {
		t.Fatalf("no IHDR at offset 12, found %q", string(out[12:16]))
	}
	binary.BigEndian.PutUint32(out[16:20], uint32(w))
	binary.BigEndian.PutUint32(out[20:24], uint32(h))
	n := int(binary.BigEndian.Uint32(out[8:12]))
	binary.BigEndian.PutUint32(out[12+4+n:12+8+n], crc32.ChecksumIEEE(out[12:12+4+n]))
	return out
}
