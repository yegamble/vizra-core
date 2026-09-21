package fixtures

import "runtime"

// LoadCorpusImage produces one synthetic photograph for the DECLARED LOAD
// CORPUS (VZ-OPS-007). It deliberately shares the painter with the correctness
// corpus so that the budget runs measure the same kind of content, but nothing
// it produces is hashed into fixtures/manifest.json — see cmd/loadcorpusgen for
// why the two corpora are kept apart.
func LoadCorpusImage(w, h int, seed uint64, quality int) ([]byte, error) {
	return encodeJPEG(scene(w, h, seed), quality)
}

// GoVersion is the toolchain the corpus was built with. The manifest pins it
// because the standard library's encoders are what write the bytes.
func GoVersion() string { return runtime.Version() }
