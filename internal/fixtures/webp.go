package fixtures

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"image/color"
)

// Animated WebP, written here rather than by libwebp.
//
// A WebP frame is a VP8 or VP8L bitstream. VP8 is arithmetic-coded against
// probability tables that run to a thousand values and cannot be written
// responsibly from memory; VP8L (the lossless codec) can, because of one
// property of its entropy coding: a Huffman code declared with a single symbol
// consumes ZERO bits per occurrence. A solid-colour VP8L frame is therefore a
// fixed 32-bit header, five one-symbol code declarations, and no pixel data at
// all — about thirteen bytes, every one of which is written below.
//
// That is enough for what the fixture is FOR: an animated WebP is a distinct
// container and a distinct decoder path from GIF and from a still WebP, and the
// pipeline has to demonstrate that it preserves the animation.

// ---------------------------------------------------------------------------
// bit writer: VP8L packs bits least-significant-first within each byte
// ---------------------------------------------------------------------------

type bitWriter struct {
	buf []byte
	cur uint32
	n   uint
}

func (w *bitWriter) write(v uint32, bits uint) {
	for i := uint(0); i < bits; i++ {
		w.cur |= ((v >> i) & 1) << w.n
		w.n++
		if w.n == 8 {
			w.buf = append(w.buf, byte(w.cur))
			w.cur, w.n = 0, 0
		}
	}
}

func (w *bitWriter) bytes() []byte {
	if w.n > 0 {
		w.buf = append(w.buf, byte(w.cur))
		w.cur, w.n = 0, 0
	}
	return w.buf
}

type bitReader struct {
	b   []byte
	pos uint
	err error
}

func (r *bitReader) read(bits uint) uint32 {
	var v uint32
	for i := uint(0); i < bits; i++ {
		byteIdx := (r.pos + i) / 8
		if int(byteIdx) >= len(r.b) {
			r.err = errors.New("VP8L bitstream ended early")
			return v
		}
		bit := (uint32(r.b[byteIdx]) >> ((r.pos + i) % 8)) & 1
		v |= bit << i
	}
	r.pos += bits
	return v
}

// ---------------------------------------------------------------------------
// VP8L: a solid-colour lossless frame
// ---------------------------------------------------------------------------

const vp8lSignature = 0x2F

// vp8lSolid encodes a w x h lossless frame of one colour.
func vp8lSolid(w, h int, c color.NRGBA) ([]byte, error) {
	if w < 1 || h < 1 || w > 1<<14 || h > 1<<14 {
		return nil, fmt.Errorf("VP8L dimensions out of range: %dx%d", w, h)
	}
	bw := &bitWriter{}
	bw.write(uint32(w-1), 14)
	bw.write(uint32(h-1), 14)
	if c.A < 0xFF {
		bw.write(1, 1) // alpha_is_used
	} else {
		bw.write(0, 1)
	}
	bw.write(0, 3) // version 0

	bw.write(0, 1) // no transform
	bw.write(0, 1) // no colour cache
	bw.write(0, 1) // no meta-Huffman image

	// Five Huffman codes: green (with length and cache codes), red, blue,
	// alpha, distance. Each is declared in the "simple code" form with exactly
	// one symbol, which is what makes every pixel cost zero bits.
	simple := func(symbol uint32) {
		bw.write(1, 1)      // simple code
		bw.write(0, 1)      // num_symbols - 1 == 0
		bw.write(1, 1)      // the symbol is stored in 8 bits
		bw.write(symbol, 8) // the symbol
	}
	simple(uint32(c.G))
	simple(uint32(c.R))
	simple(uint32(c.B))
	simple(uint32(c.A))
	simple(0) // distance code; the alphabet is 40 symbols, so 0 is in range

	return append([]byte{vp8lSignature}, bw.bytes()...), nil
}

// vp8lReadSolid parses what vp8lSolid writes, from the bitstream, so the
// property test does not have to trust the encoder.
func vp8lReadSolid(b []byte) (w, h int, c color.NRGBA, err error) {
	if len(b) < 6 || b[0] != vp8lSignature {
		return 0, 0, c, errors.New("not a VP8L bitstream")
	}
	r := &bitReader{b: b[1:]}
	w = int(r.read(14)) + 1
	h = int(r.read(14)) + 1
	alphaUsed := r.read(1) == 1
	if v := r.read(3); v != 0 {
		return 0, 0, c, fmt.Errorf("VP8L version is %d, want 0", v)
	}
	if r.read(1) != 0 {
		return 0, 0, c, errors.New("this reader does not handle VP8L transforms")
	}
	if r.read(1) != 0 {
		return 0, 0, c, errors.New("this reader does not handle a colour cache")
	}
	if r.read(1) != 0 {
		return 0, 0, c, errors.New("this reader does not handle a meta-Huffman image")
	}
	sym := make([]uint32, 5)
	for i := range sym {
		if r.read(1) != 1 {
			return 0, 0, c, fmt.Errorf("Huffman code %d is not a simple code", i)
		}
		if n := r.read(1); n != 0 {
			return 0, 0, c, fmt.Errorf("Huffman code %d declares %d symbols, want 1", i, n+1)
		}
		if r.read(1) != 1 {
			return 0, 0, c, fmt.Errorf("Huffman code %d does not use an 8-bit symbol", i)
		}
		sym[i] = r.read(8)
	}
	if r.err != nil {
		return 0, 0, c, r.err
	}
	c = color.NRGBA{G: uint8(sym[0]), R: uint8(sym[1]), B: uint8(sym[2]), A: uint8(sym[3])}
	if alphaUsed != (c.A < 0xFF) {
		return 0, 0, c, fmt.Errorf("alpha_is_used is %v but the alpha symbol is %d", alphaUsed, c.A)
	}
	return w, h, c, nil
}

// ---------------------------------------------------------------------------
// RIFF container
// ---------------------------------------------------------------------------

func riffChunk(w *bytes.Buffer, fourCC string, payload []byte) {
	w.WriteString(fourCC)
	_ = binary.Write(w, binary.LittleEndian, uint32(len(payload)))
	w.Write(payload)
	if len(payload)%2 == 1 {
		w.WriteByte(0) // chunks are padded to an even size
	}
}

func put24(w *bytes.Buffer, v uint32) {
	w.WriteByte(byte(v))
	w.WriteByte(byte(v >> 8))
	w.WriteByte(byte(v >> 16))
}

// VP8X feature flags.
const (
	vp8xAnimation = 0x02
	vp8xAlpha     = 0x10
)

var animatedWebPFrames = []struct {
	c        color.NRGBA
	duration uint32
}{
	{color.NRGBA{R: 0x33, G: 0x66, B: 0x99, A: 0xFF}, 120},
	{color.NRGBA{R: 0xCC, G: 0x44, B: 0x22, A: 0x80}, 120},
	{color.NRGBA{R: 0x11, G: 0xAA, B: 0x55, A: 0x40}, 160},
}

func buildAnimatedWebP() ([]byte, error) {
	const dim = 32

	body := new(bytes.Buffer)

	// VP8X: the extended-format header that declares this an animation.
	vp8x := new(bytes.Buffer)
	vp8x.WriteByte(vp8xAnimation | vp8xAlpha)
	vp8x.Write([]byte{0, 0, 0}) // reserved
	put24(vp8x, uint32(dim-1))  // canvas width - 1
	put24(vp8x, uint32(dim-1))  // canvas height - 1
	riffChunk(body, "VP8X", vp8x.Bytes())

	// ANIM: background colour and loop count.
	anim := new(bytes.Buffer)
	anim.Write([]byte{0x00, 0x00, 0x00, 0x00})             // background BGRA: transparent
	_ = binary.Write(anim, binary.LittleEndian, uint16(0)) // loop forever
	riffChunk(body, "ANIM", anim.Bytes())

	for _, f := range animatedWebPFrames {
		frame, err := vp8lSolid(dim, dim, f.c)
		if err != nil {
			return nil, err
		}
		anmf := new(bytes.Buffer)
		put24(anmf, 0)             // frame x / 2
		put24(anmf, 0)             // frame y / 2
		put24(anmf, uint32(dim-1)) // frame width - 1
		put24(anmf, uint32(dim-1)) // frame height - 1
		put24(anmf, f.duration)    // duration, ms
		anmf.WriteByte(0)          // blending: alpha-blend; disposal: none
		riffChunk(anmf, "VP8L", frame)
		riffChunk(body, "ANMF", anmf.Bytes())
	}

	out := new(bytes.Buffer)
	out.WriteString("RIFF")
	_ = binary.Write(out, binary.LittleEndian, uint32(4+body.Len()))
	out.WriteString("WEBP")
	out.Write(body.Bytes())
	return out.Bytes(), nil
}

// ---------------------------------------------------------------------------
// property assertions
// ---------------------------------------------------------------------------

type riffChunkRef struct {
	fourCC  string
	payload []byte
}

func scanRIFF(b []byte) ([]riffChunkRef, error) {
	if len(b) < 12 || string(b[:4]) != "RIFF" || string(b[8:12]) != "WEBP" {
		return nil, errors.New("not a RIFF/WEBP file")
	}
	if got, want := int(binary.LittleEndian.Uint32(b[4:8])), len(b)-8; got != want {
		return nil, fmt.Errorf("the RIFF size field says %d, the file has %d bytes after it", got, want)
	}
	var out []riffChunkRef
	i := 12
	for i+8 <= len(b) {
		cc := string(b[i : i+4])
		n := int(binary.LittleEndian.Uint32(b[i+4 : i+8]))
		if i+8+n > len(b) {
			return nil, fmt.Errorf("chunk %s runs past the end", cc)
		}
		out = append(out, riffChunkRef{fourCC: cc, payload: b[i+8 : i+8+n]})
		i += 8 + n
		if n%2 == 1 {
			i++
		}
	}
	return out, nil
}

func verifyAnimatedWebP(b []byte) error {
	chunks, err := scanRIFF(b)
	if err != nil {
		return err
	}
	if len(chunks) < 3 {
		return fmt.Errorf("%d chunks, want VP8X, ANIM and at least one ANMF", len(chunks))
	}
	if chunks[0].fourCC != "VP8X" {
		return fmt.Errorf("the first chunk is %s, want VP8X", chunks[0].fourCC)
	}
	flags := chunks[0].payload[0]
	if flags&vp8xAnimation == 0 {
		return errors.New("the VP8X ANIMATION flag is not set, so this is not an animated WebP")
	}
	if flags&vp8xAlpha == 0 {
		return errors.New("the VP8X ALPHA flag is not set")
	}
	if chunks[1].fourCC != "ANIM" {
		return fmt.Errorf("the second chunk is %s, want ANIM", chunks[1].fourCC)
	}
	if loop := binary.LittleEndian.Uint16(chunks[1].payload[4:6]); loop != 0 {
		return fmt.Errorf("loop count is %d, want 0 (infinite)", loop)
	}
	var frames int
	for _, c := range chunks[2:] {
		if c.fourCC != "ANMF" {
			return fmt.Errorf("unexpected chunk %s after ANIM", c.fourCC)
		}
		if len(c.payload) < 16+8 {
			return errors.New("ANMF is too short to hold a frame header and a sub-chunk")
		}
		dur := uint32(c.payload[12]) | uint32(c.payload[13])<<8 | uint32(c.payload[14])<<16
		if dur == 0 {
			return fmt.Errorf("frame %d has a zero duration", frames)
		}
		sub := c.payload[16:]
		if string(sub[:4]) != "VP8L" {
			return fmt.Errorf("frame %d carries a %s sub-chunk, want VP8L", frames, string(sub[:4]))
		}
		n := int(binary.LittleEndian.Uint32(sub[4:8]))
		if 8+n > len(sub) {
			return fmt.Errorf("frame %d sub-chunk runs past the ANMF payload", frames)
		}
		w, h, col, err := vp8lReadSolid(sub[8 : 8+n])
		if err != nil {
			return fmt.Errorf("frame %d: %w", frames, err)
		}
		if w != 32 || h != 32 {
			return fmt.Errorf("frame %d decodes to %dx%d, want 32x32", frames, w, h)
		}
		if want := animatedWebPFrames[frames].c; col != want {
			return fmt.Errorf("frame %d decodes to %v, want %v", frames, col, want)
		}
		frames++
	}
	if frames != len(animatedWebPFrames) {
		return fmt.Errorf("%d frames, want %d", frames, len(animatedWebPFrames))
	}
	return nil
}
