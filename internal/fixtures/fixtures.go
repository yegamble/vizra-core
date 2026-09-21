// Package fixtures synthesises the M0 correctness fixture corpus of ADR-009.
//
// # Why this package writes every byte itself
//
// ADR-009 requires a corpus that is "synthesised deterministically by a pinned
// script", so that its provenance is the script, no licence attaches to it, and
// "the fixture changed" is a diff rather than a mystery. Every later media
// assertion — upload validation, EXIF/ICC/orientation handling, derivatives,
// GPS stripping, decoder resource bounds — is compared against these bytes, so
// if the bytes are not reproducible then none of those assertions is
// falsifiable.
//
// ADR-009 names libvips and exiftool as the tools. Neither can deliver
// byte-identical reproduction, which is the property the corpus exists for:
//
//   - libvips delegates encoding to libjpeg-turbo, libspng, libwebp and libaom.
//     Those emit different bytes across versions, across build options, and in
//     some cases across CPU architectures.
//   - exiftool writes its own version and a processing timestamp into the files
//     it touches unless told not to, and its tag ordering is not contractual.
//
// ADR-001 anticipates exactly this and carves it out: "No lane that produces
// derivative or hash evidence may use a non-libvips decoder; a pure-Go decoder
// path exists only to generate fixtures."
//
// So this package uses the Go standard library only. Pixels, EXIF and GPS IFDs,
// PNG chunks, GIF blocks, the RIFF/VP8L bitstream and the ISOBMFF box tree are
// all written here. The one thing outside this package that can move the bytes
// is the Go toolchain itself (compress/flate, image/jpeg, image/gif and
// image/png are stable in practice but not contractually frozen across
// releases), so the manifest pins it and the verifier fails on a mismatch.
//
// # The two fixtures that cannot be pure Go
//
// AVIF requires an AV1 encoder and WebM requires VP8/VP9/AV1. All three are
// multi-symbol arithmetic codecs whose default probability tables run to
// thousands of values; there is no pure-Go encoder for any of them, and
// hand-writing one is not something to do from memory. Those two files were
// produced once by recorded libvips and ffmpeg invocations — libvips heifsave
// (libheif/aom) for the AVIF, ffmpeg for the WebM — over a PNG that THIS
// generator produced, and are committed under internal/fixtures/codec as embedded
// generator inputs (see codec.go). No third-party photograph and no downloaded
// image is involved, and because the bytes are committed the whole corpus still
// reproduces byte-identically on every platform.
package fixtures

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// Version is the generator version. Bump it whenever a change to this package
// changes the bytes of any fixture; the manifest records it, so a corpus and a
// manifest from different generator versions cannot be silently combined.
const Version = "1"

// Tier says how a fixture's bytes come into being.
type Tier string

const (
	// TierNative: every byte written by this package from the Go standard
	// library. Reproduces byte-identically on any OS and any CPU architecture
	// for a given Go toolchain.
	TierNative Tier = "native"
	// TierCodec: the bytes are a committed generator input, because the format
	// needs an AV1/VP8 encoder that does not exist in pure Go. Reproduces
	// byte-identically everywhere because it is copied, not re-encoded; see
	// Manifest.CodecInputs for what produced it.
	TierCodec Tier = "codec"
)

// Decode records what a media pipeline is expected to do with a fixture. It is
// part of the ledger's success case for VZ-FOUND-007 ("expected decode outcome
// per file") and is what stops a fixture being a file whose only property is
// its hash.
type Decode string

const (
	DecodeOK             Decode = "ok"                 // decodes to the declared pixels
	DecodeRejectTruncate Decode = "reject:truncated"   // must fail: the stream ends mid-scan
	DecodeRejectPixels   Decode = "reject:pixel-limit" // must be refused on declared pixel count, BEFORE decode
	DecodeRejectExpand   Decode = "reject:byte-budget" // must be refused on decompressed size
	DecodeRejectType     Decode = "reject:type"        // must be refused: sniffed type disagrees with the claim
)

// Spec is one fixture: what it is, what it is FOR, and what must be true of it.
type Spec struct {
	ID       string   `json:"id"`
	Path     string   `json:"path"`
	Format   string   `json:"format"`
	Tier     Tier     `json:"tier"`
	ADR009   int      `json:"adr009_item"`
	Purpose  string   `json:"purpose"`
	Decode   Decode   `json:"expected_decode"`
	Width    int      `json:"width"`
	Height   int      `json:"height"`
	Asserts  []string `json:"asserts"`
	License  string   `json:"license"`
	build    func() ([]byte, error)
	verifyFn func([]byte) error
}

const genLicense = "Synthesised by vizra-core internal/fixtures. No third-party material; no licence attaches."

// ADR009Count is how many fixtures ADR-009 names. It is a CONSTANT and not
// `len(Corpus())` on purpose.
//
// Verifier finding V-2 (docs/evidence/warroom/2026-09-21-vizra-core-pr5-fixtures-VERIFY.md):
// `VerifyAgainstManifest` compared the manifest to the generator in both
// directions, and both sides shrink together — drop a spec from Corpus(),
// re-pin, and `make fixtures-verify` printed `ok — 11 fixtures`; with Corpus()
// returning nil it printed `ok — 0 fixtures`. Nothing anchored either side to
// the number ADR-009 actually names. This constant is that anchor, and the CLI
// path reads it, so "ok" means what its text claims.
//
// `TestTheCorpusIsTheTwelveOfADR009` keeps its own literal 12 rather than
// reading this constant: a check that reads the value it is checking is not a
// check, and the whole point of V-2 is that two things which move together
// anchor nothing.
const ADR009Count = 12

// Corpus returns the twelve M0 fixtures of ADR-009, in ADR order. The order is
// fixed because the manifest is a committed, diffable file.
func Corpus() []Spec {
	return []Spec{
		{
			ID: "jpeg-exif-orientation6-gps", Path: "jpeg-exif-orientation6-gps.jpg",
			Format: "jpeg", Tier: TierNative, ADR009: 1,
			Purpose: "EXIF orientation must be applied to derivatives and GPS must never survive into one (ADR-006).",
			Decode:  DecodeOK, Width: 64, Height: 48,
			Asserts: []string{
				"APP1 Exif header is the first marker after SOI",
				"EXIF Orientation (0x0112) is 6 (rotate 90 CW)",
				"a GPS IFD is present with GPSLatitude, GPSLongitude and both refs",
				"the stored pixel dimensions are 64x48, so an orientation-aware pipeline must report 48x64",
			},
			License: genLicense, build: buildOrientationGPSJPEG, verifyFn: verifyOrientationGPSJPEG,
		},
		{
			ID: "png-alpha", Path: "png-alpha.png",
			Format: "png", Tier: TierNative, ADR009: 2,
			Purpose: "Transparency must be preserved through derivatives (ADR-006).",
			Decode:  DecodeOK, Width: 96, Height: 96,
			Asserts: []string{
				"IHDR colour type 6 (truecolour with alpha), bit depth 8",
				"the image contains fully transparent, partially transparent and fully opaque pixels",
			},
			License: genLicense, build: buildAlphaPNG, verifyFn: verifyAlphaPNG,
		},
		{
			ID: "jpeg-truncated", Path: "jpeg-truncated.jpg",
			Format: "jpeg", Tier: TierNative, ADR009: 3,
			Purpose: "A partial upload must be rejected, not stored as a half image.",
			Decode:  DecodeRejectTruncate, Width: 64, Height: 48,
			Asserts: []string{
				"starts with a valid JPEG SOI and SOF0",
				"has NO EOI marker",
				"image.Decode fails with a truncated-stream error (unexpected EOF, or image/jpeg's \"short Huffman data\")",
			},
			License: genLicense, build: buildTruncatedJPEG, verifyFn: verifyTruncatedJPEG,
		},
		{
			ID: "png-oversized-dimensions", Path: "png-oversized-dimensions.png",
			Format: "png", Tier: TierNative, ADR009: 4,
			Purpose: "The pixel-count guard must refuse on DECLARED dimensions, before any pixel buffer is allocated.",
			Decode:  DecodeRejectPixels, Width: oversizedDim, Height: oversizedDim,
			Asserts: []string{
				fmt.Sprintf("IHDR declares %dx%d = 900 megapixels", oversizedDim, oversizedDim),
				"the file itself stays under 512 KiB, so size is not a usable proxy for the risk",
				"the PNG stream is valid: image.DecodeConfig succeeds and reports the declared dimensions",
			},
			License: genLicense, build: buildOversizedPNG, verifyFn: verifyOversizedPNG,
		},
		{
			ID: "png-decoder-bomb", Path: "png-decoder-bomb.png",
			Format: "png", Tier: TierNative, ADR009: 5,
			Purpose: "The decoder byte budget must bound the DECOMPRESSED stream, not the file.",
			Decode:  DecodeRejectExpand, Width: bombDim, Height: bombDim,
			Asserts: []string{
				fmt.Sprintf("modest declared dimensions (%dx%d = %.1f megapixels), so the pixel-count guard alone does not catch it", bombDim, bombDim, float64(bombDim*bombDim)/1e6),
				"the IDAT stream inflates by a factor of at least 500",
				"the decompressed raster exceeds 64 MiB",
			},
			License: genLicense, build: buildBombPNG, verifyFn: verifyBombPNG,
		},
		{
			ID: "polyglot-gif-svg-active", Path: "polyglot-gif-svg-active.svg",
			Format: "polyglot", Tier: TierNative, ADR009: 6,
			Purpose: "Content sniffing and the claimed type disagree, and the payload carries active content: never trust the client's MIME or filename (AGENTS.md).",
			Decode:  DecodeRejectType, Width: 4, Height: 4,
			Asserts: []string{
				"the file is named .svg",
				"the first six bytes are GIF89a and the prefix is a VALID, decodable GIF",
				"http.DetectContentType reports image/gif",
				"the tail after the GIF trailer is a well-formed XML svg document",
				"that document contains a <script> element, an onload handler and a remote xlink:href (SSRF bait)",
			},
			License: genLicense, build: buildPolyglot, verifyFn: verifyPolyglot,
		},
		{
			ID: "gif-animated", Path: "gif-animated.gif",
			Format: "gif", Tier: TierNative, ADR009: 7,
			Purpose: "Animation must be preserved as a first-frame thumbnail plus an animated display (ADR-006).",
			Decode:  DecodeOK, Width: 64, Height: 64,
			Asserts: []string{
				"4 frames",
				"a per-frame delay is set on every frame",
				"a NETSCAPE2.0 application extension sets an infinite loop count",
				"the frames are not identical to each other",
			},
			License: genLicense, build: buildAnimatedGIF, verifyFn: verifyAnimatedGIF,
		},
		{
			ID: "webp-animated", Path: "webp-animated.webp",
			Format: "webp", Tier: TierNative, ADR009: 8,
			Purpose: "Animated WebP is a distinct decoder path from GIF and from still WebP; it also carries alpha.",
			Decode:  DecodeOK, Width: 32, Height: 32,
			Asserts: []string{
				"RIFF/WEBP container",
				"a VP8X chunk with the ANIMATION and ALPHA flags set",
				"an ANIM chunk with an infinite loop count",
				"3 ANMF frames, each with a non-zero duration and a VP8L sub-chunk",
				"each VP8L frame decodes to its declared solid colour (checked by an independent VP8L reader in the property test)",
			},
			License: genLicense, build: buildAnimatedWebP, verifyFn: verifyAnimatedWebP,
		},
		{
			ID: "avif-still", Path: "avif-still.avif",
			Format: "avif", Tier: TierCodec, ADR009: 9,
			Purpose: "AVIF is in the format matrix (ADR-006); this is the still-image AV1 path.",
			Decode:  DecodeOK, Width: 64, Height: 64,
			Asserts: []string{
				"ftyp major brand is avif",
				"a meta box carries an ipco with an av1C configuration and an ispe declaring 64x64",
				"an mdat box carries the AV1 OBUs",
			},
			License: genLicense, build: buildAVIF, verifyFn: verifyAVIF,
		},
		{
			ID: "jpeg-12mp-budget", Path: "jpeg-12mp-budget.jpg",
			Format: "jpeg", Tier: TierNative, ADR009: 10,
			Purpose: "The derivative-set budget of ADR-009 is quoted for a 12 MP JPEG; this is that JPEG.",
			Decode:  DecodeOK, Width: budgetW, Height: budgetH,
			Asserts: []string{
				fmt.Sprintf("%dx%d = 12.0 megapixels exactly", budgetW, budgetH),
				"baseline (SOF0) JPEG, 3 components",
				"the encoded size is between 1 MiB and 8 MiB, so the budget run is not measuring a flat colour",
			},
			License: genLicense, build: buildBudgetJPEG, verifyFn: verifyBudgetJPEG,
		},
		{
			ID: "mp4-short", Path: "mp4-short.mp4",
			Format: "mp4", Tier: TierNative, ADR009: 11,
			Purpose: "Video probe and poster extraction operate on a container, not on a pixel decoder in the api (ADR-006).",
			Decode:  DecodeOK, Width: 160, Height: 120,
			Asserts: []string{
				"ISOBMFF: ftyp, moov and mdat boxes, in that order",
				"one trak with an hdlr handler_type of vide",
				"a stsd sample entry of jpeg declaring 160x120",
				"12 samples over a 1.000 s declared duration",
				"every sample is an independently decodable JPEG",
			},
			License: genLicense, build: buildMP4, verifyFn: verifyMP4,
		},
		{
			ID: "webm-short", Path: "webm-short.webm",
			Format: "webm", Tier: TierCodec, ADR009: 12,
			Purpose: "The second video container; a different demuxer from MP4.",
			Decode:  DecodeOK, Width: 64, Height: 64,
			Asserts: []string{
				"EBML header with DocType webm",
				"a Segment with a Tracks element",
				"a video track whose CodecID is V_VP8",
				"at least one SimpleBlock cluster",
			},
			License: genLicense, build: buildWebM, verifyFn: verifyWebM,
		},
	}
}

// Generate writes the whole corpus into dir and returns each file's sha256,
// keyed by fixture path. It creates dir if needed and overwrites what is there.
func Generate(dir string) (map[string]FileInfo, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	out := make(map[string]FileInfo)
	for _, s := range Corpus() {
		b, err := s.build()
		if err != nil {
			return nil, fmt.Errorf("fixture %s: %w", s.ID, err)
		}
		// Every fixture is checked for what it is FOR at the moment it is
		// generated, so a corpus can never be written that merely hashes.
		if err := s.verifyFn(b); err != nil {
			return nil, fmt.Errorf("fixture %s failed its own property assertions: %w", s.ID, err)
		}
		p := filepath.Join(dir, s.Path)
		if err := os.WriteFile(p, b, 0o644); err != nil {
			return nil, err
		}
		sum := sha256.Sum256(b)
		out[s.Path] = FileInfo{SHA256: hex.EncodeToString(sum[:]), Bytes: len(b)}
	}
	return out, nil
}

// Verify runs the property assertions of every fixture against the bytes in
// dir. It is what makes "the fixture is what it claims to be" a check rather
// than a comment.
func Verify(dir string) error {
	var problems []string
	for _, s := range Corpus() {
		b, err := os.ReadFile(filepath.Join(dir, s.Path))
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", s.ID, err))
			continue
		}
		if err := s.verifyFn(b); err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", s.ID, err))
		}
	}
	sort.Strings(problems)
	if len(problems) > 0 {
		return fmt.Errorf("fixture property assertions failed:\n  %s", joinLines(problems))
	}
	return nil
}

func joinLines(s []string) string {
	out := ""
	for i, l := range s {
		if i > 0 {
			out += "\n  "
		}
		out += l
	}
	return out
}
