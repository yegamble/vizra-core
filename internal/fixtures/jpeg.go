package fixtures

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"strings"
)

// Budget fixture dimensions: exactly 12.0 megapixels, the size ADR-009 quotes
// the derivative-set budget for.
const (
	budgetW = 4000
	budgetH = 3000
)

// ---------------------------------------------------------------------------
// EXIF
// ---------------------------------------------------------------------------
//
// Written here rather than by exiftool, which stamps its own version and a
// processing time into the file and makes no promise about tag ordering — both
// of which destroy byte-identical reproduction. See the package comment.

// TIFF/EXIF field types.
const (
	typeByte       = 1
	typeASCII      = 2
	typeShort      = 3
	typeLong       = 4
	typeRational   = 5
	tagMake        = 0x010F
	tagModel       = 0x0110
	tagOrientation = 0x0112
	tagGPSIFD      = 0x8825
)

type ifdEntry struct {
	tag   uint16
	typ   uint16
	count uint32
	// inline is used when the value fits in the 4-byte value field.
	inline [4]byte
	// payload is used otherwise; it is placed in the data area and the value
	// field becomes an offset to it.
	payload []byte
	// deferred is set when the value is an offset to another IFD, patched
	// after layout.
	deferredIFD bool
}

func short(v uint16) ifdEntry {
	var e ifdEntry
	e.typ, e.count = typeShort, 1
	// Big-endian ("MM"): a SHORT occupies the first two bytes of the field.
	binary.BigEndian.PutUint16(e.inline[0:2], v)
	return e
}

func ascii(s string) ifdEntry {
	b := append([]byte(s), 0)
	e := ifdEntry{typ: typeASCII, count: uint32(len(b))}
	if len(b) <= 4 {
		copy(e.inline[:], b)
	} else {
		e.payload = b
	}
	return e
}

func bytesEntry(b []byte) ifdEntry {
	e := ifdEntry{typ: typeByte, count: uint32(len(b))}
	if len(b) <= 4 {
		copy(e.inline[:], b)
	} else {
		e.payload = b
	}
	return e
}

// rationals encodes n unsigned rationals; always > 4 bytes, so always payload.
func rationals(vals ...[2]uint32) ifdEntry {
	buf := new(bytes.Buffer)
	for _, v := range vals {
		_ = binary.Write(buf, binary.BigEndian, v[0])
		_ = binary.Write(buf, binary.BigEndian, v[1])
	}
	return ifdEntry{typ: typeRational, count: uint32(len(vals)), payload: buf.Bytes()}
}

// buildEXIF returns a complete APP1 payload: "Exif\0\0" + a big-endian TIFF
// structure holding IFD0 and a GPS IFD.
//
// Layout is computed explicitly instead of being appended, because every offset
// in the file is relative to the start of the TIFF header and a mistake there
// produces a file that a decoder silently ignores — which is exactly the kind
// of fixture that makes a later "GPS was stripped" assertion pass for the wrong
// reason.
func buildEXIF() []byte {
	// IFD0, tags in ascending order as the TIFF spec requires.
	ifd0 := []struct {
		tag   uint16
		entry ifdEntry
	}{
		{tagMake, ascii("Vizra")},
		{tagModel, ascii("fixturegen synthetic camera")},
		// Orientation 6 = the image must be rotated 90° clockwise for display.
		// A pipeline that ignores it renders this fixture sideways, and that is
		// the whole point of the fixture.
		{tagOrientation, short(6)},
		{tagGPSIFD, ifdEntry{typ: typeLong, count: 1, deferredIFD: true}},
	}

	// GPS IFD. These are real-looking coordinates for a place that is not a
	// person's home: 51° 28' 40.12" N, 0° 0' 5.31" W — the Royal Observatory,
	// Greenwich. Nothing here is anyone's location.
	gps := []struct {
		tag   uint16
		entry ifdEntry
	}{
		{0x0000, bytesEntry([]byte{2, 3, 0, 0})}, // GPSVersionID
		{0x0001, ascii("N")},                     // GPSLatitudeRef
		{0x0002, rationals([2]uint32{51, 1}, [2]uint32{28, 1}, [2]uint32{4012, 100})}, // GPSLatitude
		{0x0003, ascii("W")}, // GPSLongitudeRef
		{0x0004, rationals([2]uint32{0, 1}, [2]uint32{0, 1}, [2]uint32{531, 100})}, // GPSLongitude
		{0x0005, bytesEntry([]byte{0})},                                            // GPSAltitudeRef: above sea level
		{0x0006, rationals([2]uint32{4700, 100})},                                  // GPSAltitude
	}

	const tiffHeaderLen = 8
	ifd0Len := 2 + 12*len(ifd0) + 4
	gpsLen := 2 + 12*len(gps) + 4
	gpsOff := tiffHeaderLen + ifd0Len
	dataOff := gpsOff + gpsLen

	// Place payloads in a fixed order: IFD0's, then the GPS IFD's.
	data := new(bytes.Buffer)
	place := func(e *ifdEntry) uint32 {
		off := uint32(dataOff + data.Len())
		data.Write(e.payload)
		// Payloads are word-aligned so that offsets stay stable if a value's
		// length changes parity.
		if data.Len()%2 == 1 {
			data.WriteByte(0)
		}
		return off
	}

	writeIFD := func(w *bytes.Buffer, entries []struct {
		tag   uint16
		entry ifdEntry
	}, gpsOffset uint32) {
		_ = binary.Write(w, binary.BigEndian, uint16(len(entries)))
		for _, it := range entries {
			e := it.entry
			_ = binary.Write(w, binary.BigEndian, it.tag)
			_ = binary.Write(w, binary.BigEndian, e.typ)
			_ = binary.Write(w, binary.BigEndian, e.count)
			switch {
			case e.deferredIFD:
				_ = binary.Write(w, binary.BigEndian, gpsOffset)
			case e.payload != nil:
				_ = binary.Write(w, binary.BigEndian, place(&e))
			default:
				w.Write(e.inline[:])
			}
		}
		_ = binary.Write(w, binary.BigEndian, uint32(0)) // no next IFD
	}

	ifd0Buf := new(bytes.Buffer)
	writeIFD(ifd0Buf, ifd0, uint32(gpsOff))
	gpsBuf := new(bytes.Buffer)
	writeIFD(gpsBuf, gps, 0)

	out := new(bytes.Buffer)
	out.WriteString("Exif\x00\x00")
	out.WriteString("MM")                                   // big-endian
	_ = binary.Write(out, binary.BigEndian, uint16(0x002A)) // TIFF magic
	_ = binary.Write(out, binary.BigEndian, uint32(8))      // offset of IFD0
	out.Write(ifd0Buf.Bytes())
	out.Write(gpsBuf.Bytes())
	out.Write(data.Bytes())
	return out.Bytes()
}

// spliceAPP1 inserts an APP1 segment immediately after the SOI marker, which is
// where EXIF must live.
func spliceAPP1(jpegBytes, payload []byte) ([]byte, error) {
	if len(jpegBytes) < 2 || jpegBytes[0] != 0xFF || jpegBytes[1] != 0xD8 {
		return nil, errors.New("not a JPEG: no SOI")
	}
	if len(payload)+2 > 0xFFFF {
		return nil, errors.New("APP1 payload too large")
	}
	out := new(bytes.Buffer)
	out.Write(jpegBytes[:2])
	out.Write([]byte{0xFF, 0xE1})
	_ = binary.Write(out, binary.BigEndian, uint16(len(payload)+2))
	out.Write(payload)
	out.Write(jpegBytes[2:])
	return out.Bytes(), nil
}

// ---------------------------------------------------------------------------
// builders
// ---------------------------------------------------------------------------

func encodeJPEG(img image.Image, quality int) ([]byte, error) {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func buildOrientationGPSJPEG() ([]byte, error) {
	base, err := encodeJPEG(scene(64, 48, 1), 90)
	if err != nil {
		return nil, err
	}
	return spliceAPP1(base, buildEXIF())
}

func buildTruncatedJPEG() ([]byte, error) {
	full, err := encodeJPEG(scene(64, 48, 2), 90)
	if err != nil {
		return nil, err
	}
	// Cut inside the entropy-coded scan: past SOS, well before EOI. A fixed
	// fraction keeps the cut point deterministic and independent of the exact
	// encoded length.
	cut := len(full)*3/4 + 1
	if cut >= len(full)-2 {
		return nil, errors.New("truncation point is not inside the scan")
	}
	return append([]byte(nil), full[:cut]...), nil
}

func buildBudgetJPEG() ([]byte, error) {
	return encodeJPEG(scene(budgetW, budgetH, 10), 85)
}

// ---------------------------------------------------------------------------
// property assertions
// ---------------------------------------------------------------------------

// jpegMarkers walks the JPEG marker structure. It is a reader written for the
// tests, independent of the writer above, so that a bug in the writer cannot
// validate itself.
type jpegInfo struct {
	app1    []byte
	hasEOI  bool
	sofCode byte
	w, h    int
	comps   int
}

func scanJPEG(b []byte) (jpegInfo, error) {
	var info jpegInfo
	if len(b) < 2 || b[0] != 0xFF || b[1] != 0xD8 {
		return info, errors.New("no SOI")
	}
	i := 2
	for i+3 < len(b) {
		if b[i] != 0xFF {
			return info, fmt.Errorf("expected a marker at offset %d, found 0x%02X", i, b[i])
		}
		m := b[i+1]
		if m == 0xD9 {
			info.hasEOI = true
			return info, nil
		}
		size := int(binary.BigEndian.Uint16(b[i+2 : i+4]))
		if size < 2 || i+2+size > len(b) {
			return info, errors.New("segment runs past the end of the file")
		}
		seg := b[i+4 : i+2+size]
		switch {
		case m == 0xE1:
			info.app1 = seg
		case m >= 0xC0 && m <= 0xCF && m != 0xC4 && m != 0xC8 && m != 0xCC:
			info.sofCode = m
			if len(seg) >= 6 {
				info.h = int(binary.BigEndian.Uint16(seg[1:3]))
				info.w = int(binary.BigEndian.Uint16(seg[3:5]))
				info.comps = int(seg[5])
			}
		}
		i += 2 + size
		if m == 0xDA { // SOS: the entropy-coded scan follows; look for EOI at the end.
			info.hasEOI = len(b) >= 2 && b[len(b)-2] == 0xFF && b[len(b)-1] == 0xD9
			return info, nil
		}
	}
	return info, nil
}

// exifTags parses the TIFF structure of an APP1 payload into tag -> raw value,
// per IFD. Again: an independent reader, not the writer run backwards.
type exifDump struct {
	ifd0 map[uint16][]byte
	gps  map[uint16][]byte
}

func parseEXIF(app1 []byte) (exifDump, error) {
	var d exifDump
	if len(app1) < 14 || string(app1[:6]) != "Exif\x00\x00" {
		return d, errors.New("APP1 is not an Exif segment")
	}
	t := app1[6:]
	if string(t[:2]) != "MM" || binary.BigEndian.Uint16(t[2:4]) != 0x002A {
		return d, errors.New("TIFF header is not big-endian 0x002A")
	}
	readIFD := func(off uint32) (map[uint16][]byte, uint32, error) {
		if int(off)+2 > len(t) {
			return nil, 0, errors.New("IFD offset past end")
		}
		n := int(binary.BigEndian.Uint16(t[off : off+2]))
		m := make(map[uint16][]byte, n)
		var gpsOff uint32
		var prevTag int = -1
		for i := 0; i < n; i++ {
			p := int(off) + 2 + i*12
			if p+12 > len(t) {
				return nil, 0, errors.New("IFD entry past end")
			}
			tag := binary.BigEndian.Uint16(t[p : p+2])
			if int(tag) <= prevTag {
				return nil, 0, fmt.Errorf("IFD tags are not in ascending order at tag 0x%04X", tag)
			}
			prevTag = int(tag)
			typ := binary.BigEndian.Uint16(t[p+2 : p+4])
			cnt := binary.BigEndian.Uint32(t[p+4 : p+8])
			var size uint32
			switch typ {
			case typeByte, typeASCII:
				size = cnt
			case typeShort:
				size = cnt * 2
			case typeLong:
				size = cnt * 4
			case typeRational:
				size = cnt * 8
			default:
				return nil, 0, fmt.Errorf("unexpected field type %d", typ)
			}
			var val []byte
			if size <= 4 {
				val = t[p+8 : p+8+int(size)]
			} else {
				o := binary.BigEndian.Uint32(t[p+8 : p+12])
				if int(o)+int(size) > len(t) {
					return nil, 0, fmt.Errorf("value offset for tag 0x%04X runs past the end", tag)
				}
				val = t[o : o+size]
			}
			if tag == tagGPSIFD {
				gpsOff = binary.BigEndian.Uint32(t[p+8 : p+12])
			}
			m[tag] = val
		}
		return m, gpsOff, nil
	}
	ifd0, gpsOff, err := readIFD(binary.BigEndian.Uint32(t[4:8]))
	if err != nil {
		return d, err
	}
	d.ifd0 = ifd0
	if gpsOff == 0 {
		return d, errors.New("no GPS IFD pointer")
	}
	gps, _, err := readIFD(gpsOff)
	if err != nil {
		return d, fmt.Errorf("GPS IFD: %w", err)
	}
	d.gps = gps
	return d, nil
}

func verifyOrientationGPSJPEG(b []byte) error {
	info, err := scanJPEG(b)
	if err != nil {
		return err
	}
	if info.app1 == nil {
		return errors.New("no APP1 segment")
	}
	if info.w != 64 || info.h != 48 {
		return fmt.Errorf("stored dimensions are %dx%d, want 64x48", info.w, info.h)
	}
	// The APP1 must be the FIRST marker after SOI, or decoders that stop at
	// the first unexpected marker will not see the EXIF.
	if !bytes.Equal(b[2:4], []byte{0xFF, 0xE1}) {
		return errors.New("APP1 is not the first marker after SOI")
	}
	d, err := parseEXIF(info.app1)
	if err != nil {
		return err
	}
	o, ok := d.ifd0[tagOrientation]
	if !ok {
		return errors.New("no Orientation tag")
	}
	if got := binary.BigEndian.Uint16(o); got != 6 {
		return fmt.Errorf("Orientation is %d, want 6", got)
	}
	for _, want := range []struct {
		tag  uint16
		name string
	}{
		{0x0001, "GPSLatitudeRef"}, {0x0002, "GPSLatitude"},
		{0x0003, "GPSLongitudeRef"}, {0x0004, "GPSLongitude"},
	} {
		v, ok := d.gps[want.tag]
		if !ok || len(v) == 0 {
			return fmt.Errorf("no %s in the GPS IFD — this fixture exists to prove GPS is stripped, and it cannot prove that without GPS", want.name)
		}
	}
	if got := len(d.gps[0x0002]); got != 24 {
		return fmt.Errorf("GPSLatitude is %d bytes, want 24 (three rationals)", got)
	}
	if _, err := jpeg.Decode(bytes.NewReader(b)); err != nil {
		return fmt.Errorf("the fixture does not decode: %w", err)
	}
	return nil
}

func verifyTruncatedJPEG(b []byte) error {
	info, err := scanJPEG(b)
	if err != nil {
		return err
	}
	if info.sofCode != 0xC0 {
		return fmt.Errorf("SOF marker is 0x%02X, want 0xC0 (baseline)", info.sofCode)
	}
	if info.hasEOI {
		return errors.New("the fixture has an EOI marker, so it is not truncated")
	}
	_, err = jpeg.Decode(bytes.NewReader(b))
	if err == nil {
		return errors.New("the truncated fixture decoded successfully; it cannot prove a partial upload is rejected")
	}
	// The failure must be a TRUNCATION, not a corrupt header: a fixture that is
	// rejected for the wrong reason would let a pipeline pass this case while
	// happily storing a half-decoded image.
	//
	// image/jpeg reports a scan that ends early either as an unexpected EOF or,
	// when the entropy-coded segment simply runs out, as "short Huffman data"
	// (a jpeg.FormatError). Both mean the same thing here; anything else means
	// the fixture broke somewhere other than where it was cut.
	if !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) &&
		!strings.Contains(err.Error(), "short Huffman data") {
		return fmt.Errorf("decode failed with %q, want a truncated-stream error", err)
	}
	return nil
}

func verifyBudgetJPEG(b []byte) error {
	info, err := scanJPEG(b)
	if err != nil {
		return err
	}
	if info.w != budgetW || info.h != budgetH {
		return fmt.Errorf("dimensions are %dx%d, want %dx%d", info.w, info.h, budgetW, budgetH)
	}
	if info.w*info.h != 12_000_000 {
		return fmt.Errorf("%d pixels, want exactly 12,000,000", info.w*info.h)
	}
	if info.sofCode != 0xC0 {
		return fmt.Errorf("SOF marker is 0x%02X, want 0xC0 (baseline)", info.sofCode)
	}
	if info.comps != 3 {
		return fmt.Errorf("%d components, want 3", info.comps)
	}
	if len(b) < 1<<20 || len(b) > 8<<20 {
		return fmt.Errorf("encoded size is %d bytes; the budget fixture must be between 1 MiB and 8 MiB or the derivative run is measuring a flat colour", len(b))
	}
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(b))
	if err != nil {
		return err
	}
	if cfg.Width != budgetW || cfg.Height != budgetH {
		return fmt.Errorf("DecodeConfig reports %dx%d", cfg.Width, cfg.Height)
	}
	return nil
}
