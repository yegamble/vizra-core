package fixtures

import (
	"bytes"
	_ "embed"
	"encoding/binary"
	"errors"
	"fmt"
	"image/png"
)

// The codec tier.
//
// AVIF needs an AV1 encoder and WebM needs VP8, VP9 or AV1. All three are
// multi-symbol arithmetic codecs whose default probability tables run to
// thousands of values, there is no pure-Go encoder for any of them, and writing
// one from memory is not something to do in a fixture generator. So these two
// files were produced ONCE, by the recorded commands in CodecOrigins below,
// from source.png — which this generator produced, and which is committed
// beside them so the chain is checkable rather than asserted.
//
// They are committed as generator INPUTS, so:
//
//   - every output fixture still reproduces byte-identically on every platform
//     and architecture, including this arm64 machine, because these two are
//     copied rather than re-encoded;
//   - `make fixtures` needs nothing but a Go toolchain, so an M1 slice can
//     produce the corpus without libvips, ffmpeg or Docker;
//   - no third-party photograph and no downloaded image is in the repository:
//     the pixels are the generator's own.
//
// The cost is stated plainly rather than hidden: for these two files the
// manifest asserts the committed bytes, not a re-encode. `fixturegen
// repin-codec` re-runs the recorded commands when a codec pin genuinely moves,
// and refuses when the tools are absent or at a different version.

//go:embed codec/source.png
var codecSourcePNG []byte

//go:embed codec/avif-still.avif
var codecAVIF []byte

//go:embed codec/webm-short.webm
var codecWebM []byte

// CodecOrigin records exactly what produced a codec-tier input.
type CodecOrigin struct {
	Path        string   `json:"path"`
	SHA256      string   `json:"sha256"`
	Bytes       int      `json:"bytes"`
	Tool        string   `json:"tool"`
	ToolVersion string   `json:"tool_version"`
	Argv        []string `json:"argv"`
	Note        string   `json:"note,omitempty"`
}

// codecSourceSeed is the scene seed for source.png. The property test asserts
// that the committed source.png is byte-identical to what this generator
// produces from it, so "the encoder input was our own synthetic image" is a
// check and not a claim.
const codecSourceSeed = 9

func buildCodecSourcePNG() ([]byte, error) {
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestCompression}
	if err := enc.Encode(&buf, scene(64, 64, codecSourceSeed)); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// CodecInputs returns the provenance of every committed codec-tier input,
// including the source image the encoders were given.
func CodecInputs() []CodecOrigin {
	return []CodecOrigin{
		{
			Path: "internal/fixtures/codec/source.png",
			Tool: "vizra-core internal/fixtures", ToolVersion: "generator " + Version,
			Argv: []string{"scene(64,64,9)", "png.Encoder{CompressionLevel: BestCompression}"},
			Note: "The encoder input. TestCommittedCodecSourceIsGeneratorOutput asserts this file equals what the generator produces today.",
		},
		{
			Path: "internal/fixtures/codec/avif-still.avif",
			Tool: "libvips (heifsave, libheif/aom)", ToolVersion: "vips-8.18.2, libheif 1.21.2, darwin/arm64",
			Argv: []string{"vips", "copy", "codec/source.png", "avif-still.avif[compression=av1,Q=60,effort=4,subsample-mode=off]"},
			Note: "Repeatable on this toolchain: two consecutive runs produced identical bytes.",
		},
		{
			Path: "internal/fixtures/codec/webm-short.webm",
			Tool: "ffmpeg (libvpx VP8) + TrackUID normalisation", ToolVersion: "ffmpeg 8.1, darwin/arm64",
			Argv: []string{
				"ffmpeg", "-nostdin", "-hide_banner", "-loglevel", "error", "-y",
				"-fflags", "+bitexact", "-flags:v", "+bitexact",
				"-loop", "1", "-i", "codec/source.png", "-t", "1", "-r", "12",
				"-c:v", "libvpx", "-b:v", "200k", "-threads", "1",
				"-deadline", "good", "-cpu-used", "4", "-f", "webm", "webm-short.webm",
			},
			Note: "ffmpeg's Matroska muxer writes a RANDOM 8-byte TrackUID even with +bitexact, so two runs of the identical command differ. " +
				"The repin step rewrites every occurrence of that UID to the fixed value 56495a5241464958 (\"VIZRAFIX\"); with that normalisation the command reproduces byte-for-byte.",
		},
	}
}

// codecTrackUID is the normalised Matroska TrackUID. See CodecInputs.
var codecTrackUID = []byte{0x56, 0x49, 0x5A, 0x52, 0x41, 0x46, 0x49, 0x58}

func buildAVIF() ([]byte, error) { return append([]byte(nil), codecAVIF...), nil }
func buildWebM() ([]byte, error) { return append([]byte(nil), codecWebM...), nil }

// ---------------------------------------------------------------------------
// AVIF property assertions (ISOBMFF, reusing the MP4 box walker)
// ---------------------------------------------------------------------------

func verifyAVIF(b []byte) error {
	boxes, err := scanBoxes(b)
	if err != nil {
		return err
	}
	if len(boxes) < 2 || boxes[0].typ != "ftyp" {
		return errors.New("no ftyp box")
	}
	if got := string(boxes[0].payload[:4]); got != "avif" {
		return fmt.Errorf("ftyp major brand is %q, want avif", got)
	}
	meta, ok := findBox(boxes, "meta")
	if !ok {
		return errors.New("no meta box")
	}
	// meta is a FullBox: skip its version and flags before walking children.
	if len(meta.payload) < 4 {
		return errors.New("meta box is too short")
	}
	iprp, err := descend(meta.payload[4:], "iprp", "ipco")
	if err != nil {
		return fmt.Errorf("meta/iprp/ipco: %w", err)
	}
	props, err := scanBoxes(iprp.payload)
	if err != nil {
		return err
	}
	var sawAV1C bool
	var w, h uint32
	for _, p := range props {
		switch p.typ {
		case "av1C":
			sawAV1C = true
		case "ispe":
			if len(p.payload) >= 12 {
				w = binary.BigEndian.Uint32(p.payload[4:8])
				h = binary.BigEndian.Uint32(p.payload[8:12])
			}
		}
	}
	if !sawAV1C {
		return errors.New("no av1C configuration property: this is not an AV1-coded image")
	}
	if w != 64 || h != 64 {
		return fmt.Errorf("ispe declares %dx%d, want 64x64", w, h)
	}
	if _, ok := findBox(boxes, "mdat"); !ok {
		return errors.New("no mdat box: the file declares an image and carries no coded data")
	}
	return nil
}

// ---------------------------------------------------------------------------
// WebM property assertions (EBML)
// ---------------------------------------------------------------------------

// ebmlReader walks EBML elements. IDs are read with their length marker intact,
// which is how Matroska IDs are conventionally written (0x1A45DFA3 and so on).
type ebmlReader struct {
	b   []byte
	pos int
}

func vintLen(first byte) int {
	for i := 0; i < 8; i++ {
		if first&(0x80>>i) != 0 {
			return i + 1
		}
	}
	return 0
}

func (r *ebmlReader) element() (id uint64, payload []byte, err error) {
	if r.pos >= len(r.b) {
		return 0, nil, errors.New("end of stream")
	}
	n := vintLen(r.b[r.pos])
	if n == 0 || r.pos+n > len(r.b) {
		return 0, nil, errors.New("malformed element id")
	}
	for i := 0; i < n; i++ {
		id = id<<8 | uint64(r.b[r.pos+i])
	}
	r.pos += n

	if r.pos >= len(r.b) {
		return 0, nil, errors.New("truncated element size")
	}
	sn := vintLen(r.b[r.pos])
	if sn == 0 || r.pos+sn > len(r.b) {
		return 0, nil, errors.New("malformed element size")
	}
	size := uint64(r.b[r.pos] & (0xFF >> sn))
	unknown := size == uint64(0xFF>>sn)
	for i := 1; i < sn; i++ {
		size = size<<8 | uint64(r.b[r.pos+i])
		if r.b[r.pos+i] != 0xFF {
			unknown = false
		}
	}
	r.pos += sn
	if unknown || r.pos+int(size) > len(r.b) {
		// An unknown-size element (a live-muxed Segment) runs to the end.
		payload = r.b[r.pos:]
		r.pos = len(r.b)
		return id, payload, nil
	}
	payload = r.b[r.pos : r.pos+int(size)]
	r.pos += int(size)
	return id, payload, nil
}

// findEBML searches a level for an element id, descending into the master
// elements named in the path.
func findEBML(b []byte, path ...uint64) ([]byte, error) {
	cur := b
	for i, want := range path {
		r := &ebmlReader{b: cur}
		var found []byte
		for r.pos < len(r.b) {
			id, payload, err := r.element()
			if err != nil {
				break
			}
			if id == want {
				found = payload
				break
			}
		}
		if found == nil {
			return nil, fmt.Errorf("no EBML element 0x%X at level %d", want, i)
		}
		cur = found
	}
	return cur, nil
}

const (
	ebmlHeader   = 0x1A45DFA3
	ebmlDocType  = 0x4282
	ebmlSegment  = 0x18538067
	ebmlTracks   = 0x1654AE6B
	ebmlEntry    = 0xAE
	ebmlCodecID  = 0x86
	ebmlTrackUID = 0x73C5
	ebmlCluster  = 0x1F43B675
	ebmlSimpleBk = 0xA3
	ebmlVideo    = 0xE0
	ebmlPixelW   = 0xB0
	ebmlPixelH   = 0xBA
)

func verifyWebM(b []byte) error {
	hdr, err := findEBML(b, ebmlHeader)
	if err != nil {
		return err
	}
	dt, err := findEBML(hdr, ebmlDocType)
	if err != nil {
		return err
	}
	if got := string(bytes.TrimRight(dt, "\x00")); got != "webm" {
		return fmt.Errorf("DocType is %q, want webm", got)
	}
	seg, err := findEBML(b, ebmlSegment)
	if err != nil {
		return err
	}
	entry, err := findEBML(seg, ebmlTracks, ebmlEntry)
	if err != nil {
		return err
	}
	codec, err := findEBML(entry, ebmlCodecID)
	if err != nil {
		return err
	}
	if got := string(bytes.TrimRight(codec, "\x00")); got != "V_VP8" {
		return fmt.Errorf("CodecID is %q, want V_VP8", got)
	}
	uid, err := findEBML(entry, ebmlTrackUID)
	if err != nil {
		return err
	}
	if !bytes.Equal(uid, codecTrackUID) {
		return fmt.Errorf("TrackUID is %x, want the normalised %x — without that normalisation this file is not reproducible", uid, codecTrackUID)
	}
	video, err := findEBML(entry, ebmlVideo)
	if err != nil {
		return err
	}
	pw, err := findEBML(video, ebmlPixelW)
	if err != nil {
		return err
	}
	ph, err := findEBML(video, ebmlPixelH)
	if err != nil {
		return err
	}
	if uintFromEBML(pw) != 64 || uintFromEBML(ph) != 64 {
		return fmt.Errorf("pixel dimensions are %dx%d, want 64x64", uintFromEBML(pw), uintFromEBML(ph))
	}
	cluster, err := findEBML(seg, ebmlCluster)
	if err != nil {
		return fmt.Errorf("no cluster: a video with no frames is not a video: %w", err)
	}
	if _, err := findEBML(cluster, ebmlSimpleBk); err != nil {
		return fmt.Errorf("no SimpleBlock in the first cluster: %w", err)
	}
	return nil
}

func uintFromEBML(b []byte) uint64 {
	var v uint64
	for _, c := range b {
		v = v<<8 | uint64(c)
	}
	return v
}
