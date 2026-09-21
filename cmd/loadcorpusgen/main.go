// Command loadcorpusgen produces the DECLARED LOAD CORPUS of ADR-009 /
// VZ-OPS-007: a parameterised set of synthetic photographs for the budget runs
// on the reference host.
//
// It is deliberately a separate program from cmd/fixturegen, and its output is
// deliberately NOT committed and NOT in fixtures/manifest.json:
//
//   - the correctness corpus is twelve small files whose exact bytes every
//     later media assertion is compared against, and it must be reproducible
//     byte-for-byte;
//   - the load corpus is ten thousand files measured in gigabytes, and what
//     matters about it is its DECLARED shape — file count, total bytes and
//     megapixel mix — not any individual file's hash.
//
// Mixing them would put gigabytes of generated photographs into a manifest that
// is supposed to be reviewable in a diff.
//
// Usage:
//
//	loadcorpusgen -out DIR [-count 10000] [-seed 1] [-mix 12,8,4] [-quality 85] [-dry-run]
//
// The run writes DIR/corpus.json recording exactly what it produced, which is
// the declaration ADR-009 asks for.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/yegamble/vizra-core/internal/fixtures"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "loadcorpusgen: %v\n", err)
		os.Exit(1)
	}
}

type declaration struct {
	Generator     string            `json:"generator"`
	GeneratorVer  string            `json:"generator_version"`
	Toolchain     string            `json:"toolchain"`
	Seed          uint64            `json:"seed"`
	Count         int               `json:"count"`
	Quality       int               `json:"quality"`
	MegapixelMix  []int             `json:"megapixel_mix"`
	TotalBytes    int64             `json:"total_bytes"`
	MeanBytes     int64             `json:"mean_bytes"`
	PerSizeCounts map[string]int    `json:"per_size_counts"`
	PerSizeBytes  map[string]int64  `json:"per_size_bytes"`
	SampleSHA256  map[string]string `json:"sample_sha256"`
	GeneratedAt   string            `json:"generated_at,omitempty"`
	Note          string            `json:"note"`
}

func run() error {
	out := flag.String("out", "", "output directory (required)")
	count := flag.Int("count", 10000, "number of photographs to generate (ADR-009 declares 10,000)")
	seed := flag.Uint64("seed", 1, "PRNG seed; the same seed produces the same corpus")
	mix := flag.String("mix", "12,8,4", "megapixel mix, comma separated; files are round-robined over it")
	quality := flag.Int("quality", 85, "JPEG quality")
	dryRun := flag.Bool("dry-run", false, "report what would be produced without writing image files")
	flag.Parse()

	if *out == "" {
		return fmt.Errorf("-out is required")
	}
	if *count < 1 {
		return fmt.Errorf("-count must be at least 1")
	}
	sizes, err := parseMix(*mix)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}

	d := declaration{
		Generator: "loadcorpusgen", GeneratorVer: fixtures.Version,
		Toolchain: goVersion(), Seed: *seed, Count: *count, Quality: *quality,
		PerSizeCounts: map[string]int{}, PerSizeBytes: map[string]int64{},
		SampleSHA256: map[string]string{},
		Note: "VZ-OPS-007 declared load corpus. Synthetic; not committed; not part of fixtures/manifest.json. " +
			"Regenerate with the same -count, -seed, -mix and -quality to get the same corpus.",
	}
	for _, s := range sizes {
		d.MegapixelMix = append(d.MegapixelMix, s.mp)
	}

	start := time.Now()
	for i := 0; i < *count; i++ {
		s := sizes[i%len(sizes)]
		name := fmt.Sprintf("load-%06d-%dmp.jpg", i, s.mp)
		b, err := fixtures.LoadCorpusImage(s.w, s.h, *seed+uint64(i), *quality)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		key := strconv.Itoa(s.mp) + "mp"
		d.PerSizeCounts[key]++
		d.PerSizeBytes[key] += int64(len(b))
		d.TotalBytes += int64(len(b))
		if i < len(sizes) {
			sum := sha256.Sum256(b)
			d.SampleSHA256[name] = hex.EncodeToString(sum[:])
		}
		if !*dryRun {
			if err := os.WriteFile(filepath.Join(*out, name), b, 0o644); err != nil {
				return err
			}
		}
	}
	d.MeanBytes = d.TotalBytes / int64(*count)

	b, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(*out, "corpus.json"), append(b, '\n'), 0o644); err != nil {
		return err
	}

	fmt.Printf("load corpus: %d files, %d bytes total, %d bytes mean, mix %v MP, seed %d, %s\n",
		d.Count, d.TotalBytes, d.MeanBytes, d.MegapixelMix, d.Seed, time.Since(start).Round(time.Millisecond))
	if *dryRun {
		fmt.Println("(-dry-run: no image files were written; corpus.json describes what would be)")
	}
	fmt.Printf("declaration: %s\n", filepath.Join(*out, "corpus.json"))
	return nil
}

type sizeSpec struct {
	mp, w, h int
}

// parseMix turns "12,8,4" into concrete 4:3 dimensions whose pixel count is the
// requested megapixels, rounded to an even width and height.
func parseMix(s string) ([]sizeSpec, error) {
	var out []sizeSpec
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		mp, err := strconv.Atoi(part)
		if err != nil || mp < 1 || mp > 200 {
			return nil, fmt.Errorf("bad megapixel value %q in -mix", part)
		}
		// 4:3 => w = sqrt(mp*1e6*4/3)
		w := isqrt(mp * 1000000 * 4 / 3)
		w -= w % 2
		h := w * 3 / 4
		h -= h % 2
		out = append(out, sizeSpec{mp: mp, w: w, h: h})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("-mix is empty")
	}
	sort.Slice(out, func(i, j int) bool { return out[i].mp > out[j].mp })
	return out, nil
}

func isqrt(n int) int {
	if n < 2 {
		return n
	}
	x := n
	y := (x + 1) / 2
	for y < x {
		x = y
		y = (x + n/x) / 2
	}
	return x
}

func goVersion() string { return fixtures.GoVersion() }
