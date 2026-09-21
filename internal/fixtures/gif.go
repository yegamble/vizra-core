package fixtures

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"net/http"
	"strings"
)

func buildAnimatedGIF() ([]byte, error) {
	const n, frames = 64, 4
	pal := make(color.Palette, 0, 8)
	for i := 0; i < 8; i++ {
		pal = append(pal, color.RGBA{R: uint8(i * 32), G: uint8(255 - i*32), B: uint8(i * 16), A: 255})
	}
	g := &gif.GIF{LoopCount: 0} // 0 = loop forever
	for f := 0; f < frames; f++ {
		img := image.NewPaletted(image.Rect(0, 0, n, n), pal)
		for y := 0; y < n; y++ {
			for x := 0; x < n; x++ {
				// A bar that moves per frame, so no two frames are equal.
				idx := ((x+f*16)/8 + y/16) % len(pal)
				img.SetColorIndex(x, y, uint8(idx))
			}
		}
		g.Image = append(g.Image, img)
		g.Delay = append(g.Delay, 10) // 100 ms
		g.Disposal = append(g.Disposal, gif.DisposalNone)
	}
	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, g); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func verifyAnimatedGIF(b []byte) error {
	g, err := gif.DecodeAll(bytes.NewReader(b))
	if err != nil {
		return err
	}
	if len(g.Image) != 4 {
		return fmt.Errorf("%d frames, want 4", len(g.Image))
	}
	for i, d := range g.Delay {
		if d <= 0 {
			return fmt.Errorf("frame %d has no delay; a zero-delay animation is not one", i)
		}
	}
	if g.LoopCount != 0 {
		return fmt.Errorf("loop count is %d, want 0 (infinite, set by a NETSCAPE2.0 extension)", g.LoopCount)
	}
	if !bytes.Contains(b, []byte("NETSCAPE2.0")) {
		return errors.New("no NETSCAPE2.0 application extension, so the loop count is not actually in the file")
	}
	// Frames must differ, or "animated" is a claim about metadata only.
	first := g.Image[0].Pix
	allSame := true
	for _, im := range g.Image[1:] {
		if !bytes.Equal(first, im.Pix) {
			allSame = false
			break
		}
	}
	if allSame {
		return errors.New("every frame is identical")
	}
	return nil
}

// ---------------------------------------------------------------------------
// polyglot
// ---------------------------------------------------------------------------

// activeSVG is the tail of the polyglot: a well-formed SVG document carrying
// three separate things an upload path must refuse to execute or to fetch.
const activeSVG = `<?xml version="1.0" encoding="UTF-8"?>
<svg xmlns="http://www.w3.org/2000/svg" xmlns:xlink="http://www.w3.org/1999/xlink"
     width="4" height="4" viewBox="0 0 4 4" onload="fixtureActiveContentRan()">
  <script type="application/ecmascript"><![CDATA[
    // Synthetic active content. If a Vizra surface ever executes this, the
    // upload path has served an SVG as a document instead of as an image.
    function fixtureActiveContentRan() { document.title = "vizra-fixture-svg-script-executed"; }
  ]]></script>
  <image xlink:href="http://169.254.169.254/latest/meta-data/" x="0" y="0" width="1" height="1"/>
  <rect width="4" height="4" fill="#336699"/>
</svg>
`

func buildPolyglot() ([]byte, error) {
	// A tiny but entirely valid GIF, so that content sniffing gets a real
	// answer rather than a guess.
	pal := color.Palette{color.RGBA{R: 0x33, G: 0x66, B: 0x99, A: 255}, color.RGBA{A: 255}}
	img := image.NewPaletted(image.Rect(0, 0, 4, 4), pal)
	for i := range img.Pix {
		img.Pix[i] = uint8(i % 2)
	}
	var head bytes.Buffer
	if err := gif.Encode(&head, img, &gif.Options{NumColors: 2}); err != nil {
		return nil, err
	}
	out := new(bytes.Buffer)
	out.Write(head.Bytes()) // ends with the 0x3B trailer
	out.WriteString("\n")
	out.WriteString(activeSVG)
	return out.Bytes(), nil
}

func verifyPolyglot(b []byte) error {
	if len(b) < 6 || string(b[:6]) != "GIF89a" {
		return errors.New("the first six bytes are not GIF89a, so content sniffing will not disagree with the .svg name")
	}
	if got := http.DetectContentType(b); got != "image/gif" {
		return fmt.Errorf("http.DetectContentType says %q, want image/gif", got)
	}
	// The GIF prefix must really decode, or the sniffed type is a lie the
	// fixture tells rather than one an attacker could.
	if _, err := gif.Decode(bytes.NewReader(b)); err != nil {
		return fmt.Errorf("the GIF prefix does not decode: %w", err)
	}
	// The trailer, then the SVG document.
	trailer := bytes.IndexByte(b, 0x3B)
	if trailer < 0 {
		return errors.New("no GIF trailer")
	}
	tail := b[trailer+1:]
	x := bytes.Index(tail, []byte("<?xml"))
	if x < 0 {
		return errors.New("no XML declaration after the GIF trailer")
	}
	doc := tail[x:]
	var root struct {
		XMLName xml.Name
	}
	if err := xml.Unmarshal(doc, &root); err != nil {
		return fmt.Errorf("the tail is not well-formed XML: %w", err)
	}
	if root.XMLName.Local != "svg" {
		return fmt.Errorf("the XML root is %q, want svg", root.XMLName.Local)
	}
	for _, want := range []string{"<script", "onload=", "169.254.169.254"} {
		if !strings.Contains(string(doc), want) {
			return fmt.Errorf("the SVG does not carry %q, so it is not an active-content fixture", want)
		}
	}
	return nil
}
