package fixtures

// The VZ-FOUND-007 supplement: fixtures the ledger outcome names that ADR-009's
// twelve do not cover. They carry ADR009 item 0 and at least one ledger
// category, so the twelve stay anchored to ADR009Count exactly as before.

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"image"
	"image/color"
	_ "image/jpeg" // register for image.Decode
	"image/png"
	"math"
	"strings"
)

// LedgerCategories is the VZ-FOUND-007 outcome list, verbatim and in order.
var LedgerCategories = []string{
	"portrait", "landscape", "exif-rotated", "icc-wide-gamut", "transparent-png", "transparent-webp",
	"animated-gif", "animated-webp", "animated-avif", "huge-dimensions", "truncated", "corrupt",
	"polyglot", "gps-tagged", "mp4", "mov", "webm", "equirectangular-360",
}

// OpenCategories are ledger categories with no fixture yet, each with the reason.
// The manifest records them so "not covered" is a reviewed line, never silence.
var OpenCategories = map[string]string{
	"animated-avif": "needs an AV1 sequence (avis) encoder; no pure-Go encoder exists and no codec-tier input has been recorded yet",
}

// categoriesOf lists the ledger categories each ADR-009 fixture already covers.
var categoriesOf = map[string][]string{
	"jpeg-exif-orientation6-gps": {"landscape", "exif-rotated", "gps-tagged"},
	"png-alpha":                  {"transparent-png"},
	"jpeg-truncated":             {"truncated"},
	"png-oversized-dimensions":   {"huge-dimensions"},
	"polyglot-gif-svg-active":    {"polyglot"},
	"gif-animated":               {"animated-gif"},
	"webp-animated":              {"animated-webp"},
	"mp4-short":                  {"mp4"},
	"webm-short":                 {"webm"},
}

// DecodeRejectCorrupt: the stream is complete but its integrity check fails.
const DecodeRejectCorrupt Decode = "reject:corrupt"

func supplement() []Spec {
	return []Spec{
		{
			ID: "jpeg-portrait", Path: "jpeg-portrait.jpg", Format: "jpeg", Tier: TierNative,
			Categories: []string{"portrait"},
			Purpose:    "A portrait image with NO orientation tag: the stored dimensions are the display dimensions.",
			Decode:     DecodeOK, Width: 48, Height: 64,
			Asserts: []string{"baseline JPEG, 48x64 (height > width)", "no APP1 Exif segment"},
			License: genLicense, Provenance: "fixturegen: buildPortraitJPEG (internal/fixtures/supplement.go), scene seed 21, quality 85",
			build: buildPortraitJPEG, verifyFn: verifyPortraitJPEG,
		},
		{
			ID: "jpeg-icc-display-p3", Path: "jpeg-icc-display-p3.jpg", Format: "jpeg", Tier: TierNative,
			Categories: []string{"icc-wide-gamut"},
			Purpose:    "A wide-gamut ICC profile must be honoured or converted, never silently dropped.",
			Decode:     DecodeOK, Width: 64, Height: 48,
			Asserts: []string{
				"an APP2 ICC_PROFILE segment, sequence 1 of 1",
				"the profile header size equals the embedded length, signature acsp, class mntr, space RGB, PCS XYZ",
				"the rXYZ colorant X exceeds sRGB's 0.4361 (Display P3 primaries, D50-adapted)",
			},
			License: genLicense, Provenance: "fixturegen: buildICCJPEG + displayP3Profile (internal/fixtures/supplement.go), synthesised ICC v4 profile, scene seed 22",
			build: buildICCJPEG, verifyFn: verifyICCJPEG,
		},
		{
			ID: "webp-alpha-still", Path: "webp-alpha-still.webp", Format: "webp", Tier: TierNative,
			Categories: []string{"transparent-webp"},
			Purpose:    "Still lossless WebP with alpha: a different decoder path from the animated one.",
			Decode:     DecodeOK, Width: 24, Height: 16,
			Asserts: []string{"RIFF/WEBP with a single VP8L chunk and no ANIM", "VP8L alpha_is_used is set and the pixel alpha is 0x80"},
			License: genLicense, Provenance: "fixturegen: buildAlphaWebP (internal/fixtures/supplement.go), VP8L solid NRGBA{0x20,0x90,0xE0,0x80}",
			build: buildAlphaWebP, verifyFn: verifyAlphaWebP,
		},
		{
			ID: "png-corrupt-crc", Path: "png-corrupt-crc.png", Format: "png", Tier: TierNative,
			Categories: []string{"corrupt"},
			Purpose:    "A complete file whose integrity check fails must be rejected, distinct from a truncated one.",
			Decode:     DecodeRejectCorrupt, Width: 32, Height: 32,
			Asserts: []string{"all chunks present through IEND", "the IDAT CRC does not match its data", "png.Decode fails with a checksum error"},
			License: genLicense, Provenance: "fixturegen: buildCorruptPNG (internal/fixtures/supplement.go), scene seed 23, IDAT CRC xor 0xFFFFFFFF",
			build: buildCorruptPNG, verifyFn: verifyCorruptPNG,
		},
		{
			ID: "jpeg-equirectangular-360", Path: "jpeg-equirectangular-360.jpg", Format: "jpeg", Tier: TierNative,
			Categories: []string{"equirectangular-360"},
			Purpose:    "A 360 panorama must be recognised from its GPano XMP, not guessed from the 2:1 ratio alone.",
			Decode:     DecodeOK, Width: 128, Height: 64,
			Asserts: []string{"2:1 aspect ratio", "an APP1 XMP packet with GPano:ProjectionType equirectangular and full-pano dimensions equal to the image"},
			License: genLicense, Provenance: "fixturegen: buildEquirectJPEG (internal/fixtures/supplement.go), scene seed 24, XMP written by the generator",
			build: buildEquirectJPEG, verifyFn: verifyEquirectJPEG,
		},
		{
			ID: "mov-short", Path: "mov-short.mov", Format: "mov", Tier: TierNative,
			Categories: []string{"mov"},
			Purpose:    "QuickTime is sniffed by its ftyp brand; it must take the MOV path, not be mislabelled MP4.",
			Decode:     DecodeOK, Width: 160, Height: 120,
			Asserts: []string{"ftyp major brand is \"qt  \" with only qt compatible brands", "the same ftyp/moov/mdat tree as mp4-short: one vide trak, 12 JPEG samples"},
			License: genLicense, Provenance: "fixturegen: buildMOV (internal/fixtures/supplement.go) = buildMP4 with a same-length QuickTime ftyp",
			build: buildMOV, verifyFn: verifyMOV,
		},
	}
}

func buildPortraitJPEG() ([]byte, error) { return encodeJPEG(scene(48, 64, 21), 85) }

func decodeDims(b []byte, w, h int) error {
	img, _, err := image.Decode(bytes.NewReader(b))
	if err != nil {
		return fmt.Errorf("does not decode: %w", err)
	}
	if got := img.Bounds().Size(); got.X != w || got.Y != h {
		return fmt.Errorf("decodes to %dx%d, want %dx%d", got.X, got.Y, w, h)
	}
	return nil
}

func verifyPortraitJPEG(b []byte) error {
	info, err := scanJPEG(b)
	if err != nil {
		return err
	}
	if info.app1 != nil {
		return errors.New("carries an APP1 segment; the portrait fixture must have no orientation tag")
	}
	return decodeDims(b, 48, 64)
}

// spliceSegment inserts marker+payload right after SOI.
func spliceSegment(jpegBytes []byte, marker byte, payload []byte) ([]byte, error) {
	if len(jpegBytes) < 2 || jpegBytes[0] != 0xFF || jpegBytes[1] != 0xD8 || len(payload)+2 > 0xFFFF {
		return nil, errors.New("not a JPEG or segment too large")
	}
	out := new(bytes.Buffer)
	out.Write(jpegBytes[:2])
	out.Write([]byte{0xFF, marker})
	_ = binary.Write(out, binary.BigEndian, uint16(len(payload)+2))
	out.Write(payload)
	out.Write(jpegBytes[2:])
	return out.Bytes(), nil
}

// segments returns every marker segment payload before SOS with the given marker.
func segments(b []byte, marker byte) [][]byte {
	var out [][]byte
	for i := 2; i+4 <= len(b) && b[i] == 0xFF; {
		m := b[i+1]
		n := int(binary.BigEndian.Uint16(b[i+2:]))
		if m == 0xDA || i+2+n > len(b) {
			break
		}
		if m == marker {
			out = append(out, b[i+4:i+2+n])
		}
		i += 2 + n
	}
	return out
}

func s15(v float64) []byte { return be32(uint32(int32(math.Round(v * 65536)))) }

func xyzTag(x, y, z float64) []byte {
	return concat([]byte("XYZ "), be32(0), s15(x), s15(y), s15(z))
}

// displayP3Profile synthesises a minimal ICC v4 RGB display profile with
// Display P3 primaries (D50-adapted) and a linear TRC.
func displayP3Profile() []byte {
	desc := func(s string) []byte {
		var u []byte
		for _, r := range s {
			u = append(u, byte(r>>8), byte(r))
		}
		return concat([]byte("mluc"), be32(0, 1, 12), []byte("enUS"), be32(uint32(len(u)), 28), u)
	}
	curv := concat([]byte("curv"), be32(0, 1), be16(0x0100, 0)) // gamma 1.0, padded
	tags := []struct {
		sig  string
		data []byte
	}{
		{"desc", desc("Vizra synthetic Display P3")},
		{"cprt", desc("No copyright; synthesised")},
		{"wtpt", xyzTag(0.9642, 1.0, 0.8249)},
		{"rXYZ", xyzTag(0.515121, 0.241196, -0.001053)},
		{"gXYZ", xyzTag(0.291977, 0.692245, 0.041885)},
		{"bXYZ", xyzTag(0.157104, 0.066574, 0.784073)},
		{"rTRC", curv}, {"gTRC", curv}, {"bTRC", curv},
	}
	table := be32(uint32(len(tags)))
	var data []byte
	off := 128 + 4 + 12*len(tags)
	for _, t := range tags {
		for len(data)%4 != 0 {
			data = append(data, 0)
		}
		table = concat(table, []byte(t.sig), be32(uint32(off+len(data)), uint32(len(t.data))))
		data = append(data, t.data...)
	}
	for len(data)%4 != 0 {
		data = append(data, 0)
	}
	size := 128 + len(table) + len(data)
	hdr := concat(be32(uint32(size)), make([]byte, 4), be32(0x04300000), []byte("mntrRGB XYZ "),
		be16(2026, 1, 1, 0, 0, 0), []byte("acsp"), make([]byte, 24), be32(0, 0), // platform..rendering intent
		s15(0.9642), s15(1.0), s15(0.8249), make([]byte, 4+16+28))
	return concat(hdr[:128], table, data)
}

func buildICCJPEG() ([]byte, error) {
	base, err := encodeJPEG(scene(64, 48, 22), 85)
	if err != nil {
		return nil, err
	}
	return spliceSegment(base, 0xE2, concat([]byte("ICC_PROFILE\x00"), []byte{1, 1}, displayP3Profile()))
}

func verifyICCJPEG(b []byte) error {
	var prof []byte
	for _, s := range segments(b, 0xE2) {
		if bytes.HasPrefix(s, []byte("ICC_PROFILE\x00")) && len(s) > 14 && s[12] == 1 && s[13] == 1 {
			prof = s[14:]
		}
	}
	if prof == nil || len(prof) < 132 {
		return errors.New("no APP2 ICC_PROFILE segment 1/1")
	}
	if int(binary.BigEndian.Uint32(prof)) != len(prof) {
		return errors.New("ICC header size disagrees with the embedded length")
	}
	if string(prof[36:40]) != "acsp" || string(prof[12:24]) != "mntrRGB XYZ " {
		return errors.New("ICC header is not an acsp mntr RGB/XYZ profile")
	}
	n := int(binary.BigEndian.Uint32(prof[128:]))
	for i := 0; i < n && 132+12*i+12 <= len(prof); i++ {
		e := prof[132+12*i:]
		if string(e[:4]) == "rXYZ" {
			o := int(binary.BigEndian.Uint32(e[4:]))
			if o+12 > len(prof) {
				return errors.New("rXYZ out of range")
			}
			x := float64(int32(binary.BigEndian.Uint32(prof[o+8:]))) / 65536
			if x <= 0.4361 {
				return fmt.Errorf("rXYZ X is %.4f, not wider than sRGB", x)
			}
			return decodeDims(b, 64, 48)
		}
	}
	return errors.New("no rXYZ tag")
}

var alphaWebPColour = [4]uint8{0x20, 0x90, 0xE0, 0x80}

func buildAlphaWebP() ([]byte, error) {
	c := alphaWebPColour
	v, err := vp8lSolid(24, 16, color.NRGBA{R: c[0], G: c[1], B: c[2], A: c[3]})
	if err != nil {
		return nil, err
	}
	body := new(bytes.Buffer)
	riffChunk(body, "VP8L", v)
	out := new(bytes.Buffer)
	out.WriteString("RIFF")
	_ = binary.Write(out, binary.LittleEndian, uint32(4+body.Len()))
	out.WriteString("WEBP")
	out.Write(body.Bytes())
	return out.Bytes(), nil
}

func verifyAlphaWebP(b []byte) error {
	chunks, err := scanRIFF(b)
	if err != nil {
		return err
	}
	if len(chunks) != 1 || chunks[0].fourCC != "VP8L" {
		return fmt.Errorf("want exactly one VP8L chunk, got %d chunk(s)", len(chunks))
	}
	w, h, c, err := vp8lReadSolid(chunks[0].payload)
	if err != nil {
		return err
	}
	if w != 24 || h != 16 || c.A != 0x80 {
		return fmt.Errorf("VP8L is %dx%d alpha 0x%02X, want 24x16 alpha 0x80", w, h, c.A)
	}
	return nil
}

func buildCorruptPNG() ([]byte, error) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, scene(32, 32, 23)); err != nil {
		return nil, err
	}
	b := buf.Bytes()
	i := bytes.Index(b, []byte("IDAT"))
	if i < 4 {
		return nil, errors.New("no IDAT")
	}
	n := int(binary.BigEndian.Uint32(b[i-4:]))
	crc := i + 4 + n
	binary.BigEndian.PutUint32(b[crc:], binary.BigEndian.Uint32(b[crc:])^0xFFFFFFFF)
	return b, nil
}

func verifyCorruptPNG(b []byte) error {
	if !bytes.HasSuffix(b, []byte("IEND\xAE\x42\x60\x82")) {
		return errors.New("does not end with a valid IEND chunk; it would be truncated, not corrupt")
	}
	i := bytes.Index(b, []byte("IDAT"))
	if i < 4 {
		return errors.New("no IDAT")
	}
	n := int(binary.BigEndian.Uint32(b[i-4:]))
	if crc32.ChecksumIEEE(b[i:i+4+n]) == binary.BigEndian.Uint32(b[i+4+n:]) {
		return errors.New("the IDAT CRC is valid; the fixture is not corrupt")
	}
	if _, err := png.Decode(bytes.NewReader(b)); err == nil || !strings.Contains(err.Error(), "checksum") {
		return fmt.Errorf("png.Decode error is %v, want a checksum error", err)
	}
	return nil
}

const xmpNS = "http://ns.adobe.com/xap/1.0/\x00"

func buildEquirectJPEG() ([]byte, error) {
	base, err := encodeJPEG(scene(128, 64, 24), 85)
	if err != nil {
		return nil, err
	}
	xmp := `<x:xmpmeta xmlns:x="adobe:ns:meta/"><rdf:RDF xmlns:rdf="` + "http://www.w3.org/1999/02/22-rdf-syntax-ns#" + `">` +
		`<rdf:Description xmlns:GPano="` + "http://ns.google.com/photos/1.0/panorama/" + `" GPano:ProjectionType="equirectangular"` +
		` GPano:UsePanoramaViewer="True" GPano:FullPanoWidthPixels="128" GPano:FullPanoHeightPixels="64"` +
		` GPano:CroppedAreaImageWidthPixels="128" GPano:CroppedAreaImageHeightPixels="64" GPano:CroppedAreaLeftPixels="0" GPano:CroppedAreaTopPixels="0"/>` +
		`</rdf:RDF></x:xmpmeta>`
	return spliceSegment(base, 0xE1, []byte(xmpNS+xmp))
}

func verifyEquirectJPEG(b []byte) error {
	for _, s := range segments(b, 0xE1) {
		if bytes.HasPrefix(s, []byte(xmpNS)) {
			x := string(s)
			for _, want := range []string{`GPano:ProjectionType="equirectangular"`, `GPano:FullPanoWidthPixels="128"`, `GPano:FullPanoHeightPixels="64"`} {
				if !strings.Contains(x, want) {
					return fmt.Errorf("XMP lacks %s", want)
				}
			}
			return decodeDims(b, 128, 64)
		}
	}
	return errors.New("no APP1 XMP packet")
}

var (
	mp4Ftyp = []byte("isom\x00\x00\x02\x00isomiso2mp41")
	movFtyp = []byte("qt  \x00\x00\x02\x00qt  qt  qt  ")
)

func buildMOV() ([]byte, error) {
	b, err := buildMP4()
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(b[8:28], mp4Ftyp) {
		return nil, errors.New("mp4-short ftyp is not the expected layout")
	}
	copy(b[8:28], movFtyp) // same length: every stco offset stays valid
	return b, nil
}

func verifyMOV(b []byte) error {
	if len(b) < 28 || string(b[4:8]) != "ftyp" || !bytes.Equal(b[8:28], movFtyp) {
		return errors.New("ftyp is not the QuickTime brand set")
	}
	c := append([]byte(nil), b...)
	copy(c[8:28], mp4Ftyp)
	return verifyMP4(c) // the box tree beneath the brand is the MP4 one
}
