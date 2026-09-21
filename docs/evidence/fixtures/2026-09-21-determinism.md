# VZ-FOUND-007 — where byte-identical reproduction holds, and where it does not

Recorded 2026-09-21 on the builder's machine (darwin/arm64) and, for the runs
marked CI, on the ADR-009 acceptance platform (GitHub-hosted `ubuntu-24.04`,
amd64). The manifest **of record** is the one the CI lane reproduces; this file
says what was measured locally and what was not.

## Summary

| Claim | Status |
|---|---|
| The twelve fixtures reproduce byte-identically from the generator on darwin/arm64 | MEASURED — `make fixtures-verify` green, and the whole corpus regenerated in two independent runs inside one process (`TestGenerationIsDeterministicWithinAProcess`) |
| …and on linux/amd64, the acceptance platform | The `fixtures` CI lane on this PR's head SHA; see the PR body for the run |
| arm64 and amd64 produce the SAME bytes | Compare the sha256 table below with the CI job's "Fixture corpus" step summary, which prints the same table on the runner |
| The Go **toolchain** changes the bytes | MEASURED — see below. This is why the manifest pins it and the verifier refuses to compare across it |
| The go.mod **`go` directive** changes the bytes on its own | NOT OBSERVED. `go 1.26.0` and `go 1.26.2` with the toolchain held at go1.27.1 produced identical bytes for the whole corpus and for the codec source PNG. It is still pinned — see "Why the directive is pinned anyway" |
| libvips/exiftool reproduce byte-identically | NOT CLAIMED, and the reason the generator does not use them |

## The toolchain is byte-influencing — measured

Identical generator source, identical raster (`scene(64,64,9)`, sha256
`cefbb6ef…` of the RGBA pixels in both runs), identical encoder settings
(`png.Encoder{CompressionLevel: png.BestCompression}`):

```
$ go version           # module declaring `go 1.26.2`, no toolchain line, GOTOOLCHAIN=auto
go version go1.26.2 darwin/arm64
  raster sha256 cefbb6ef5f5bd9296e3c58bbdb658b86c5f64fb4d0fb2f8c1f9e7e2cf366ac8e
  png    sha256 ae627abc8e4de10d640663eb520ef2e2c2193a29372a537b4bc72061c5d9d35d  (7227 bytes)

$ go version           # module declaring `go 1.26.0` + `toolchain go1.27.1`
go version go1.27.1 darwin/arm64
  raster sha256 cefbb6ef5f5bd9296e3c58bbdb658b86c5f64fb4d0fb2f8c1f9e7e2cf366ac8e
  png    sha256 73d5acba4491a2b9069a0f7a29d16a12c31cb3d91fdc2b83ce89657f910f3ff2  (7223 bytes)
```

Same pixels in, different file out: the difference is entirely in the deflate
stream that `compress/flate` produced. `image/jpeg`, `image/gif`, `image/png`
and `compress/flate` are what write this corpus, and none of them is
contractually frozen across Go releases.

This was found the hard way. The two committed codec inputs were first encoded
from a `source.png` built by a scratch program whose `go.mod` had no `toolchain`
line, so `GOTOOLCHAIN=auto` ran go1.26.2 while the repository runs go1.27.1.
`TestCommittedCodecSourceIsGeneratorOutput` failed with a four-byte size
difference, which is exactly the failure that test exists to produce.

**Consequence:** `fixtures-verify` compares `runtime.Version()` against the
manifest and REFUSES to compare bytes across a mismatch, rather than reporting a
dozen differences it cannot explain.

## Why the directive is pinned anyway

Measured: with the toolchain fixed at go1.27.1, moving go.mod's directive from
`go 1.26.0` to `go 1.26.2` changed nothing — all twelve fixture hashes and the
codec source PNG were identical. No claim is made that it does.

It is pinned for two mechanisms that do hold: the directive sets the GODEBUG
compatibility defaults the standard library runs under, and under the default
`GOTOOLCHAIN=auto` raising it is itself a way to make a different toolchain run
the generator — which is the difference measured above. One recorded line closes
both paths.

## Where the generator does NOT write the bytes

Two of the twelve need an encoder that does not exist in pure Go:

| Fixture | Encoder | Why not pure Go |
|---|---|---|
| `avif-still.avif` | libvips 8.18.2 (libheif 1.21.2 / aom) | AV1 is a multi-symbol arithmetic codec with default CDF tables running to thousands of values |
| `webm-short.webm` | ffmpeg 8.1 (libvpx VP8) | VP8's first partition requires 1056 update flags, each coded against a specific probability from `coeff_update_probs` |

Both were encoded ONCE from `internal/fixtures/codec/source.png` — this
generator's own output — and the results are committed as generator inputs. The
corpus therefore still reproduces byte-identically everywhere, because those two
are copied rather than re-encoded.

Reproducibility of the encoders themselves, measured on this machine:

- **libvips AVIF: reproducible.** Two consecutive runs of the recorded command
  produced identical bytes (`e521f13e…` both times).
- **ffmpeg WebM: NOT reproducible as invoked.** Two runs of the identical
  command differed in 16 bytes. The cause is the Matroska muxer's random 8-byte
  `TrackUID` (element `0x73C5`), which it writes even with `-fflags +bitexact`;
  the element appears twice in the file. Adding `+bitexact` did remove a
  differing `DateUTC`, but not this.

  The repin step therefore rewrites every occurrence of that UID to the fixed
  value `56495a5241464958` ("VIZRAFIX"). With that normalisation the command
  reproduces byte-for-byte (verified: two runs, both `2e50fda9…` after
  normalisation). `verifyWebM` asserts the normalised UID is present, so
  undoing the normalisation turns the check red.

## Why not libvips and exiftool, which ADR-009 names

- libvips delegates encoding to libjpeg-turbo, libspng, libwebp and libaom.
  Those emit different bytes across versions and build options.
- exiftool writes its own version and a processing timestamp into files it
  touches unless told otherwise, and its tag ordering is not contractual.
- exiftool is not installed on the owner's machine at all (already recorded as a
  blocker in VZ-ISSUE-001), so a generator depending on it could not be verified
  there in any form.

ADR-001 anticipates this and permits it: "No lane that produces derivative or
hash evidence may use a non-libvips decoder; **a pure-Go decoder path exists
only to generate fixtures**."

This is a refinement of ADR-009's named *tooling*, not of its decision. Its
decision — synthesised, deterministic, pinned, provenance is the script, no
licence required, manifest committed — is implemented exactly. **An ADR-009
amendment recording the tooling change is owed, and belongs to the meta repo,
not to this PR.**

## Corpus produced on darwin/arm64 at this commit

```
44863b49cf10222c13a85dca77ea7f6b1950905b5e48dfc3ff37a24c368185e4  jpeg-exif-orientation6-gps.jpg
1a1f167d06a2086ae69424a77706cb0d9635c17b870191cc28440dcd707240b0  png-alpha.png
df0c816e0c1442a0f64ef7754989164555e71ed59e02a6d09895dcef48953f9f  jpeg-truncated.jpg
e884f5303764d491fcdb5f8cd393c450bb9855e46333fa12a418eaf8e1b79c82  png-oversized-dimensions.png
3da19cc1b42f8f37d0eb52f5051a437ed4fd2becab368a2e21d7103783aca610  png-decoder-bomb.png
59bd983da145c698e45d2bcf57238e4d6b9ef258ad899350fa704e744d2ca822  polyglot-gif-svg-active.svg
b691c5125af022e1f6cefe9965697f996fcbf79485f3d0a14c80f674ac6f5ae9  gif-animated.gif
a9358ca2a446f9666093e6c6c271a7a591582fb16135ac60a9056b219f876f77  webp-animated.webp
e521f13e2659eebe55d40e33034f2310bff2e2c55ed7f40279b23033a5baf189  avif-still.avif
66e66264b03a773ad2260ee658ab5cdf6b543092719c744e7a339f9880b2e7f8  jpeg-12mp-budget.jpg
bf7018e19e7d262cf9856aeceea0f51ba34c013cb6b3f847fb09b54ac2e68b32  mp4-short.mp4
2e50fda961c2701bc9a47e03b37b45af50a5937dcd07d9f87b6b365df9542c77  webm-short.webm
```

The same table is in `fixtures/manifest.json`, the committed artefact. The CI
lane prints it again from the runner in its step summary, so the two platforms
can be compared line by line.
