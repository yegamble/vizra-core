package fixtures

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"image/jpeg"
)

// A short MP4, written as ISOBMFF boxes here rather than by ffmpeg.
//
// The video track carries Motion JPEG samples: one JPEG per frame, with the
// 'jpeg' visual sample entry. That is a real, demuxable video track whose every
// byte this generator produces — H.264, VP9 and AV1 all need an encoder whose
// output is not reproducible across versions, and MJPEG needs nothing but
// image/jpeg, which is already the encoder for two other fixtures in this
// corpus.
//
// What the fixture is FOR is the container: ADR-006 puts poster extraction and
// probing on the worker side of the codec boundary, and the upload path has to
// tell a video container from an image before anything decodes.

const (
	mp4Width     = 160
	mp4Height    = 120
	mp4Timescale = 600
	mp4Frames    = 12
	mp4Delta     = 50 // 12 frames x 50 / 600 = 1.000 s
)

func box(typ string, payload []byte) []byte {
	out := make([]byte, 0, 8+len(payload))
	out = binary.BigEndian.AppendUint32(out, uint32(8+len(payload)))
	out = append(out, typ...)
	return append(out, payload...)
}

func fullBox(typ string, version byte, flags uint32, payload []byte) []byte {
	head := []byte{version, byte(flags >> 16), byte(flags >> 8), byte(flags)}
	return box(typ, append(head, payload...))
}

func be32(vals ...uint32) []byte {
	out := make([]byte, 0, 4*len(vals))
	for _, v := range vals {
		out = binary.BigEndian.AppendUint32(out, v)
	}
	return out
}

func be16(vals ...uint16) []byte {
	out := make([]byte, 0, 2*len(vals))
	for _, v := range vals {
		out = binary.BigEndian.AppendUint16(out, v)
	}
	return out
}

// identityMatrix is the ISO/IEC 14496-12 unity transformation matrix.
var identityMatrix = be32(0x00010000, 0, 0, 0, 0x00010000, 0, 0, 0, 0x40000000)

func buildMP4() ([]byte, error) {
	// The samples: a moving bar, so the frames differ and a poster extractor
	// has something to be wrong about.
	samples := make([][]byte, 0, mp4Frames)
	for f := 0; f < mp4Frames; f++ {
		b, err := encodeJPEG(scene(mp4Width, mp4Height, uint64(100+f)), 80)
		if err != nil {
			return nil, err
		}
		samples = append(samples, b)
	}
	var mdatPayload []byte
	sizes := make([]uint32, 0, len(samples))
	for _, s := range samples {
		mdatPayload = append(mdatPayload, s...)
		sizes = append(sizes, uint32(len(s)))
	}

	ftyp := box("ftyp", append([]byte("isom"), append(be32(512), []byte("isomiso2mp41")...)...))

	// The moov box's SIZE does not depend on the chunk offset value, so it is
	// built once to learn its length and once with the real offset. That is
	// simpler than patching bytes by search, and it cannot patch the wrong
	// four bytes.
	build := func(chunkOffset uint32) []byte {
		mvhd := fullBox("mvhd", 0, 0, concat(
			be32(0, 0, mp4Timescale, mp4Frames*mp4Delta),
			be32(0x00010000), // rate 1.0
			be16(0x0100, 0),  // volume 1.0, reserved
			be32(0, 0),       // reserved
			identityMatrix,   //
			make([]byte, 24), // pre_defined
			be32(2),          // next_track_ID
		))
		tkhd := fullBox("tkhd", 0, 0x000007, concat( // enabled | in movie | in preview
			be32(0, 0, 1, 0, mp4Frames*mp4Delta),
			be32(0, 0),       // reserved
			be16(0, 0, 0, 0), // layer, alternate_group, volume (0 for video), reserved
			identityMatrix,
			be32(mp4Width<<16, mp4Height<<16), // 16.16 fixed-point display size
		))
		mdhd := fullBox("mdhd", 0, 0, concat(
			be32(0, 0, mp4Timescale, mp4Frames*mp4Delta),
			be16(0x55C4, 0), // language "und", pre_defined
		))
		hdlr := fullBox("hdlr", 0, 0, concat(
			be32(0),
			[]byte("vide"),
			make([]byte, 12),
			[]byte("VideoHandler\x00"),
		))
		vmhd := fullBox("vmhd", 0, 1, concat(be16(0), make([]byte, 6)))
		dref := fullBox("dref", 0, 0, concat(be32(1), fullBox("url ", 0, 1, nil)))
		dinf := box("dinf", dref)

		var compressor [32]byte
		copy(compressor[1:], "Vizra fixturegen MJPEG")
		compressor[0] = byte(len("Vizra fixturegen MJPEG"))
		sampleEntry := box("jpeg", concat(
			make([]byte, 6),              // reserved
			be16(1),                      // data_reference_index
			be16(0, 0),                   // pre_defined, reserved
			make([]byte, 12),             // pre_defined[3]
			be16(mp4Width, mp4Height),    //
			be32(0x00480000, 0x00480000), // 72 dpi horizontal and vertical
			be32(0),                      // reserved
			be16(1),                      // frame_count
			compressor[:],                //
			be16(0x0018),                 // depth: colour with no alpha
			[]byte{0xFF, 0xFF},           // pre_defined: -1
		))
		stsd := fullBox("stsd", 0, 0, concat(be32(1), sampleEntry))
		stts := fullBox("stts", 0, 0, be32(1, mp4Frames, mp4Delta))
		stsc := fullBox("stsc", 0, 0, be32(1, 1, mp4Frames, 1))
		stsz := fullBox("stsz", 0, 0, concat(be32(0, mp4Frames), be32(sizes...)))
		stco := fullBox("stco", 0, 0, be32(1, chunkOffset))
		stbl := box("stbl", concat(stsd, stts, stsc, stsz, stco))
		minf := box("minf", concat(vmhd, dinf, stbl))
		mdia := box("mdia", concat(mdhd, hdlr, minf))
		trak := box("trak", concat(tkhd, mdia))
		return box("moov", concat(mvhd, trak))
	}

	moovLen := len(build(0))
	// The single chunk starts at the first byte of the mdat PAYLOAD.
	chunkOffset := uint32(len(ftyp) + moovLen + 8)
	moov := build(chunkOffset)
	if len(moov) != moovLen {
		return nil, errors.New("moov length changed when the chunk offset was filled in")
	}
	return concat(ftyp, moov, box("mdat", mdatPayload)), nil
}

func concat(parts ...[]byte) []byte {
	var n int
	for _, p := range parts {
		n += len(p)
	}
	out := make([]byte, 0, n)
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// ---------------------------------------------------------------------------
// property assertions
// ---------------------------------------------------------------------------

type mp4Box struct {
	typ     string
	payload []byte
}

func scanBoxes(b []byte) ([]mp4Box, error) {
	var out []mp4Box
	i := 0
	for i+8 <= len(b) {
		size := int(binary.BigEndian.Uint32(b[i : i+4]))
		typ := string(b[i+4 : i+8])
		if size < 8 || i+size > len(b) {
			return nil, fmt.Errorf("box %s declares size %d at offset %d, which does not fit", typ, size, i)
		}
		out = append(out, mp4Box{typ: typ, payload: b[i+8 : i+size]})
		i += size
	}
	if i != len(b) {
		return nil, fmt.Errorf("%d trailing bytes after the last box", len(b)-i)
	}
	return out, nil
}

func findBox(boxes []mp4Box, typ string) (mp4Box, bool) {
	for _, bx := range boxes {
		if bx.typ == typ {
			return bx, true
		}
	}
	return mp4Box{}, false
}

// descend walks a path of container boxes, e.g. moov/trak/mdia/hdlr.
func descend(b []byte, path ...string) (mp4Box, error) {
	cur := b
	for i, want := range path {
		boxes, err := scanBoxes(cur)
		if err != nil {
			return mp4Box{}, err
		}
		bx, ok := findBox(boxes, want)
		if !ok {
			return mp4Box{}, fmt.Errorf("no %s box", want)
		}
		if i == len(path)-1 {
			return bx, nil
		}
		cur = bx.payload
	}
	return mp4Box{}, errors.New("empty path")
}

func verifyMP4(b []byte) error {
	boxes, err := scanBoxes(b)
	if err != nil {
		return err
	}
	if len(boxes) < 3 || boxes[0].typ != "ftyp" || boxes[1].typ != "moov" || boxes[2].typ != "mdat" {
		var got []string
		for _, bx := range boxes {
			got = append(got, bx.typ)
		}
		return fmt.Errorf("box order is %v, want ftyp, moov, mdat", got)
	}
	if string(boxes[0].payload[:4]) != "isom" {
		return fmt.Errorf("major brand is %q, want isom", string(boxes[0].payload[:4]))
	}

	hdlr, err := descend(b, "moov", "trak", "mdia", "hdlr")
	if err != nil {
		return err
	}
	if got := string(hdlr.payload[8:12]); got != "vide" {
		return fmt.Errorf("handler type is %q, want vide", got)
	}

	stsd, err := descend(b, "moov", "trak", "mdia", "minf", "stbl", "stsd")
	if err != nil {
		return err
	}
	entries, err := scanBoxes(stsd.payload[8:])
	if err != nil {
		return err
	}
	if len(entries) != 1 || entries[0].typ != "jpeg" {
		return fmt.Errorf("sample entry is %v, want exactly one jpeg entry", entries)
	}
	se := entries[0].payload
	if w, h := binary.BigEndian.Uint16(se[24:26]), binary.BigEndian.Uint16(se[26:28]); int(w) != mp4Width || int(h) != mp4Height {
		return fmt.Errorf("the sample entry declares %dx%d, want %dx%d", w, h, mp4Width, mp4Height)
	}

	mdhd, err := descend(b, "moov", "trak", "mdia", "mdhd")
	if err != nil {
		return err
	}
	ts := binary.BigEndian.Uint32(mdhd.payload[12:16])
	dur := binary.BigEndian.Uint32(mdhd.payload[16:20])
	if ts == 0 || dur == 0 {
		return fmt.Errorf("timescale %d duration %d: the track declares no duration", ts, dur)
	}
	if secs := float64(dur) / float64(ts); secs < 0.9 || secs > 1.1 {
		return fmt.Errorf("declared duration is %.3f s, want about 1 s", secs)
	}

	// Every sample must be an independently decodable JPEG, or the poster
	// extractor has nothing to extract.
	stsz, err := descend(b, "moov", "trak", "mdia", "minf", "stbl", "stsz")
	if err != nil {
		return err
	}
	count := int(binary.BigEndian.Uint32(stsz.payload[8:12]))
	if count != mp4Frames {
		return fmt.Errorf("%d samples, want %d", count, mp4Frames)
	}
	stco, err := descend(b, "moov", "trak", "mdia", "minf", "stbl", "stco")
	if err != nil {
		return err
	}
	off := int(binary.BigEndian.Uint32(stco.payload[8:12]))
	if off+8 > len(b) || string(b[off-8+4:off-8+8]) != "mdat" {
		return fmt.Errorf("the chunk offset %d does not point just past an mdat header", off)
	}
	for i := 0; i < count; i++ {
		n := int(binary.BigEndian.Uint32(stsz.payload[12+4*i : 16+4*i]))
		if off+n > len(b) {
			return fmt.Errorf("sample %d runs past the end of the file", i)
		}
		if _, err := jpeg.Decode(bytes.NewReader(b[off : off+n])); err != nil {
			return fmt.Errorf("sample %d is not a decodable JPEG: %w", i, err)
		}
		off += n
	}
	return nil
}
