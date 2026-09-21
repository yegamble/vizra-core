# VZ-FOUND-007 — each fixture cross-checked with INDEPENDENT third-party tools

Recorded 2026-09-21T01:29:59Z on darwin/arm64.

The in-repo property assertions (`internal/fixtures`) are written by the same
author as the writers they check. This transcript is the independent half: the
fixtures are read back by libvips, ImageMagick and ffprobe — tools that know
nothing about this generator — to show the hand-written EXIF, VP8L bitstream
and ISOBMFF box tree are real and not merely self-consistent.

These tools are NOT a build or test dependency: no CI lane needs them, and
`make fixtures` needs only a Go toolchain. They are evidence, not plumbing.

```
$ vips --version && identify -version | head -1 && ffprobe -version | head -1
vips-8.18.2
Version: ImageMagick 7.1.2-19 Q16-HDRI aarch64 23897 https://imagemagick.org
ffprobe version 8.1 Copyright (c) 2007-2026 the FFmpeg developers
```

## 1. jpeg-exif-orientation6-gps.jpg — orientation 6 and GPS really are there
```
$ vipsheader -a jpeg-exif-orientation6-gps.jpg
jpeg-exif-orientation6-gps.jpg: 64x48 uchar, 3 bands, srgb, jpegload
width: 64
height: 48
filename: jpeg-exif-orientation6-gps.jpg
exif-ifd0-Make: Vizra (Vizra, ASCII, 6 components, 6 bytes)
exif-ifd0-Model: fixturegen synthetic camera (fixturegen synthetic camera, ASCII, 28 components, 28 bytes)
exif-ifd0-Orientation: 6 (Right-top, Short, 1 components, 2 bytes)
orientation: 6
$ identify -verbose jpeg-exif-orientation6-gps.jpg | grep exif:GPS
    exif:GPSAltitude: 4700/100
    exif:GPSAltitudeRef: .
    exif:GPSInfo: 62
    exif:GPSLatitude: 51/1,28/1,4012/100
    exif:GPSLatitudeRef: N
    exif:GPSLongitude: 0/1,0/1,531/100
    exif:GPSLongitudeRef: W
    exif:GPSVersionID: ....
```
libvips reads the orientation as `6 (Right-top)` and ImageMagick reads the
full GPS IFD. That is what a later slice has to strip.

## 2. png-alpha.png
```
$ identify -format '%wx%h %[channels] %m
' png-alpha.png
96x96 srgba 4.0 PNG
```

## 4 and 5. the two PNG bombs are small on disk and enormous when decoded
```
$ identify -format '%wx%h %m %b
' png-oversized-dimensions.png png-decoder-bomb.png
30000x30000 PNG 122067B
4200x4200 PNG 69830B
```

## 6. polyglot-gif-svg-active.svg — the extension and the magic bytes disagree
```
$ file --mime-type polyglot-gif-svg-active.svg
polyglot-gif-svg-active.svg: image/gif
$ identify -format '%wx%h %m
' polyglot-gif-svg-active.svg
4x4 GIF
```
A file named `.svg` that `file` and ImageMagick both call a GIF, carrying a
`<script>` element and a link-local `xlink:href`.

## 7. gif-animated.gif
```
$ identify -format '%n frames, %wx%h, delay %T
' gif-animated.gif
4 frames, 64x64, delay 10
```

## 8. webp-animated.webp — the HAND-WRITTEN VP8L bitstream decodes

This is the one that most needed an independent reader: the RIFF container,
the VP8X/ANIM/ANMF chunks and three VP8L frames were all written bit by bit
by `internal/fixtures/webp.go`. libvips reads them through libwebp:
```
$ vipsheader -a webp-animated.webp
webp-animated.webp: 32x32 uchar, 4 bands, srgb, webpload
width: 32
height: 32
bands: 4
loop: 0
n-pages: 3
$ vips copy 'webp-animated.webp[page=N]' frameN.png   # then sample a pixel
frame0 32x32 srgba(51,102,153,1)
frame1 32x32 srgba(127,84,93,1)
frame2 32x32 srgba(99,105,90,0.996078)
```
Frame 0 reads back as `srgba(51,102,153,1)` — 0x33/0x66/0x99, exactly the
colour encoded. Frames 1 and 2 show libvips compositing each ANMF frame onto
the canvas with the alpha-blend disposal the file declares.

## 9. avif-still.avif
```
$ vipsheader avif-still.avif
avif-still.avif: 64x64 uchar, 3 bands, srgb, heifload
$ ffprobe ... avif-still.avif
codec_name=av1
width=64
height=64
```

## 10. jpeg-12mp-budget.jpg
```
$ identify -format '%wx%h %m %b
' jpeg-12mp-budget.jpg
4000x3000 JPEG 1.54269MB
```

## 11. mp4-short.mp4 — the HAND-WRITTEN ISOBMFF box tree demuxes
```
$ ffprobe -show_entries stream=codec_name,width,height,nb_frames:format=duration,format_name mp4-short.mp4
codec_name=mjpeg
width=160
height=120
nb_frames=12
format_name=mov,mp4,m4a,3gp,3g2,mj2
duration=1.000000
```
ffmpeg demuxes the container this generator wrote and finds twelve MJPEG
samples over a declared second.

## 12. webm-short.webm
```
$ ffprobe -show_entries stream=codec_name,width,height:format=duration,format_name webm-short.webm
codec_name=vp8
width=64
height=64
format_name=matroska,webm
duration=1.000000
```

## Not cross-checked here

- `jpeg-truncated.jpg` (3): its property is that it FAILS to decode; the
  in-repo assertion checks the error class, and ImageMagick likewise refuses
  it. Nothing useful for a third-party tool to report.
- `exiftool` was not used anywhere: it is not installed on this machine
  (recorded as a blocker in VZ-ISSUE-001) and the generator deliberately does
  not depend on it.
