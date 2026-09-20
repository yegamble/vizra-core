package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/yegamble/vizra-core/internal/buildinfo"
	"github.com/yegamble/vizra-core/internal/migrate"
)

func runVersion(args []string) error {
	fs := flag.NewFlagSet("version", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}

	embedded, err := migrate.EmbeddedVersion()
	if err != nil {
		return err
	}

	info := map[string]any{
		"release":        buildinfo.Release,
		"commit":         buildinfo.Commit,
		"built_at":       buildinfo.BuiltAt,
		"go_version":     buildinfo.GoVersion(),
		"schema_version": embedded,
	}
	if buildinfo.ImageDigest != "" {
		info["image_digest"] = buildinfo.ImageDigest
	}
	if buildinfo.HasLibvips() {
		info["libvips"] = map[string]any{
			"version": buildinfo.LibvipsVersion,
			"loaders": buildinfo.Loaders(),
		}
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(info)
	}

	fmt.Printf("vizra %s (%s)\n", buildinfo.Release, buildinfo.Commit)
	fmt.Printf("  built     %s\n", orNone(buildinfo.BuiltAt))
	fmt.Printf("  go        %s\n", buildinfo.GoVersion())
	fmt.Printf("  schema    %d (embedded)\n", embedded)
	fmt.Printf("  image     %s\n", orNone(buildinfo.ImageDigest))
	if buildinfo.HasLibvips() {
		fmt.Printf("  libvips   %s\n", buildinfo.LibvipsVersion)
		fmt.Printf("  loaders   %v\n", buildinfo.Loaders())
	} else {
		// Said plainly rather than left blank: "no loader list" means this
		// binary was not built into the release image, and derivative evidence
		// from it would not be reproducible (ADR-001).
		fmt.Printf("  libvips   not recorded (this binary was not built into the release image)\n")
	}
	return nil
}

func orNone(s string) string {
	if s == "" {
		return "(not recorded)"
	}
	return s
}
