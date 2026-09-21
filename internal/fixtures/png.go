package fixtures

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"io"
)

const (
	// 30000x30000 = 900 megapixels declared, in a file that stays tiny.
	oversizedDim = 30000
	// 4200x4200 = 17.6 megapixels: modest enough that a pixel-count guard tuned
	// for the oversized fixture will NOT catch this one. What catches it is a
	// budget on the decompressed stream, which this inflates to about 70 MB.
	bombDim = 4200
)

// bigRasterLevel is the deflate level for the two fixtures whose raster runs to
// tens of megabytes. BestSpeed, not BestCompression, and deliberately:
//
//   - the data is a long run of a repeating pattern, which level 1 already
//     compresses at about 920:1 (oversized) and 1011:1 (bomb);
//   - level 9 buys 13 KB on a file that is allowed 512 KB, at six times the
//     cost, and under the race detector that difference is tens of seconds on
//     every run of a REQUIRED lane;
//   - every property these two fixtures are asserted on — declared pixel count,
//     file size ceiling, inflated size, expansion ratio — holds at level 1 with
//     the same thresholds. Nothing was relaxed to buy the speed.
const bigRasterLevel = zlib.BestSpeed

var pngMagic = []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1A, '\n'}

// pngChunk writes one PNG chunk: length, type, data, CRC32 of type+data.
func pngChunk(w *bytes.Buffer, typ string, data []byte) {
	_ = binary.Write(w, binary.BigEndian, uint32(len(data)))
	start := w.Len()
	w.WriteString(typ)
	w.Write(data)
	_ = binary.Write(w, binary.BigEndian, crc32.ChecksumIEEE(w.Bytes()[start:]))
}

func ihdr(w, h int, bitDepth, colorType byte) []byte {
	b := new(bytes.Buffer)
	_ = binary.Write(b, binary.BigEndian, uint32(w))
	_ = binary.Write(b, binary.BigEndian, uint32(h))
	b.WriteByte(bitDepth)
	b.WriteByte(colorType)
	b.WriteByte(0) // compression: deflate
	b.WriteByte(0) // filter method 0
	b.WriteByte(0) // non-interlaced
	return b.Bytes()
}

// rawPNG assembles a PNG from an IHDR description and a raster whose rows are
// produced by rowFn. The raster is streamed through zlib so that a 900-megapixel
// image never needs a 900-megapixel buffer — which is also the property the
// fixture is testing for in the pipeline that will later read it.
func rawPNG(w, h int, bitDepth, colorType byte, rowBytes int, level int, rowFn func(y int, dst []byte)) ([]byte, error) {
	out := new(bytes.Buffer)
	out.Write(pngMagic)
	pngChunk(out, "IHDR", ihdr(w, h, bitDepth, colorType))

	var idat bytes.Buffer
	// The level is passed in and pinned explicitly, never left to the default:
	// the default is a property of the standard library and this corpus must
	// not move when that default does.
	zw, err := zlib.NewWriterLevel(&idat, level)
	if err != nil {
		return nil, err
	}
	row := make([]byte, 1+rowBytes) // leading filter-type byte
	for y := 0; y < h; y++ {
		// clear() rather than an element-wise loop: this runs once per row over
		// a raster of up to 112 MB, and an element-wise reset is 112 million
		// separate writes, which the race detector instruments one by one and
		// which turned `make ci` into a ten-minute job. clear() is one memclr.
		clear(row)
		rowFn(y, row[1:])
		if _, err := zw.Write(row); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	pngChunk(out, "IDAT", idat.Bytes())
	pngChunk(out, "IEND", nil)
	return out.Bytes(), nil
}

// ---------------------------------------------------------------------------
// builders
// ---------------------------------------------------------------------------

func buildAlphaPNG() ([]byte, error) {
	const n = 96
	img := image.NewNRGBA(image.Rect(0, 0, n, n))
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			// A left-to-right alpha ramp over a colour wheel, with a fully
			// transparent notch and a fully opaque bar, so that "preserves
			// transparency" has all three cases to fail on.
			a := uint8(x * 255 / (n - 1))
			switch {
			case y < n/6:
				a = 0
			case y >= n-n/6:
				a = 255
			}
			img.SetNRGBA(x, y, color.NRGBA{
				R: uint8(255 - x*255/(n-1)),
				G: uint8(y * 255 / (n - 1)),
				B: uint8((x + y) * 255 / (2 * (n - 1))),
				A: a,
			})
		}
	}
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestCompression}
	if err := enc.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func buildOversizedPNG() ([]byte, error) {
	// 1-bit greyscale: 900 megapixels of declared dimension in about 110 KiB.
	rowBytes := (oversizedDim + 7) / 8
	return rawPNG(oversizedDim, oversizedDim, 1, 0, rowBytes, bigRasterLevel, func(y int, dst []byte) {
		// A sparse pattern rather than all-zero, so the file is a real image
		// and not a stream of nothing.
		if y%1024 == 0 {
			for i := range dst {
				dst[i] = 0xAA
			}
		}
	})
}

func buildBombPNG() ([]byte, error) {
	// 8-bit RGBA at 4200x4200: 70,564,200 bytes of raster in roughly 68 KiB of
	// file. Modest dimensions, enormous decompressed stream.
	return rawPNG(bombDim, bombDim, 8, 6, bombDim*4, bigRasterLevel, func(y int, dst []byte) {})
}

// ---------------------------------------------------------------------------
// property assertions
// ---------------------------------------------------------------------------

// pngChunks walks a PNG's chunk list independently of the writer above.
type pngHeader struct {
	w, h      int
	bitDepth  byte
	colorType byte
	idat      []byte
}

func scanPNG(b []byte) (pngHeader, error) {
	var h pngHeader
	if len(b) < 8 || !bytes.Equal(b[:8], pngMagic) {
		return h, errors.New("not a PNG: bad signature")
	}
	i := 8
	seenIHDR := false
	for i+8 <= len(b) {
		n := int(binary.BigEndian.Uint32(b[i : i+4]))
		typ := string(b[i+4 : i+8])
		if i+12+n > len(b) {
			return h, fmt.Errorf("chunk %s runs past the end", typ)
		}
		data := b[i+8 : i+8+n]
		wantCRC := binary.BigEndian.Uint32(b[i+8+n : i+12+n])
		if got := crc32.ChecksumIEEE(b[i+4 : i+8+n]); got != wantCRC {
			return h, fmt.Errorf("chunk %s has a bad CRC", typ)
		}
		switch typ {
		case "IHDR":
			seenIHDR = true
			h.w = int(binary.BigEndian.Uint32(data[0:4]))
			h.h = int(binary.BigEndian.Uint32(data[4:8]))
			h.bitDepth = data[8]
			h.colorType = data[9]
		case "IDAT":
			h.idat = append(h.idat, data...)
		case "IEND":
			if !seenIHDR {
				return h, errors.New("IEND before IHDR")
			}
			return h, nil
		}
		i += 12 + n
	}
	return h, errors.New("no IEND chunk")
}

// inflatedSize streams the IDAT through zlib and returns how many bytes come
// out, without holding them.
func inflatedSize(idat []byte) (int64, error) {
	zr, err := zlib.NewReader(bytes.NewReader(idat))
	if err != nil {
		return 0, err
	}
	defer func() { _ = zr.Close() }()
	n, err := io.Copy(io.Discard, zr)
	if err != nil {
		return n, fmt.Errorf("the IDAT stream does not inflate cleanly: %w", err)
	}
	return n, nil
}

func verifyAlphaPNG(b []byte) error {
	h, err := scanPNG(b)
	if err != nil {
		return err
	}
	if h.colorType != 6 || h.bitDepth != 8 {
		return fmt.Errorf("colour type %d bit depth %d, want 6/8 (RGBA)", h.colorType, h.bitDepth)
	}
	img, err := png.Decode(bytes.NewReader(b))
	if err != nil {
		return err
	}
	bnd := img.Bounds()
	var sawZero, sawPartial, sawOpaque bool
	for y := bnd.Min.Y; y < bnd.Max.Y; y++ {
		for x := bnd.Min.X; x < bnd.Max.X; x++ {
			_, _, _, a := img.At(x, y).RGBA()
			switch {
			case a == 0:
				sawZero = true
			case a == 0xFFFF:
				sawOpaque = true
			default:
				sawPartial = true
			}
		}
	}
	if !sawZero || !sawPartial || !sawOpaque {
		return fmt.Errorf("alpha coverage incomplete: transparent=%v partial=%v opaque=%v", sawZero, sawPartial, sawOpaque)
	}
	return nil
}

func verifyOversizedPNG(b []byte) error {
	h, err := scanPNG(b)
	if err != nil {
		return err
	}
	if h.w != oversizedDim || h.h != oversizedDim {
		return fmt.Errorf("IHDR declares %dx%d, want %dx%d", h.w, h.h, oversizedDim, oversizedDim)
	}
	if px := int64(h.w) * int64(h.h); px != 900_000_000 {
		return fmt.Errorf("%d declared pixels, want 900,000,000", px)
	}
	if len(b) > 512*1024 {
		return fmt.Errorf("the fixture is %d bytes; it must stay under 512 KiB or file size becomes a usable proxy for the risk it models", len(b))
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(b))
	if err != nil {
		return fmt.Errorf("DecodeConfig must succeed — the guard has to refuse on the DECLARED size, not on a parse error: %w", err)
	}
	if cfg.Width != oversizedDim || cfg.Height != oversizedDim {
		return fmt.Errorf("DecodeConfig reports %dx%d", cfg.Width, cfg.Height)
	}
	return nil
}

func verifyBombPNG(b []byte) error {
	h, err := scanPNG(b)
	if err != nil {
		return err
	}
	if h.w != bombDim || h.h != bombDim {
		return fmt.Errorf("IHDR declares %dx%d, want %dx%d", h.w, h.h, bombDim, bombDim)
	}
	if px := int64(h.w) * int64(h.h); px > 20_000_000 {
		return fmt.Errorf("%d declared pixels: too many. This fixture must slip past a pixel-count guard so that the BYTE budget is what catches it", px)
	}
	n, err := inflatedSize(h.idat)
	if err != nil {
		return err
	}
	want := int64(bombDim) * int64(1+bombDim*4)
	if n != want {
		return fmt.Errorf("the IDAT inflates to %d bytes, want %d", n, want)
	}
	if n < 64<<20 {
		return fmt.Errorf("the decompressed raster is %d bytes; it must exceed 64 MiB", n)
	}
	ratio := n / int64(len(h.idat))
	if ratio < 500 {
		return fmt.Errorf("the IDAT expansion ratio is %d:1, want at least 500:1", ratio)
	}
	return nil
}
