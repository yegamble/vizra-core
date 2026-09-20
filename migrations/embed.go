// Package migrations embeds the SQL migration files into the binary, so a
// release image carries exactly the schema it was built with and `vizra
// migrate` needs no files on disk (ADR-002 § Migration discipline).
//
// The .sql files live here rather than in internal/ because three other tools
// read them from this path: sqlc.yaml (`schema: migrations`),
// scripts/migrate-lint.sh and the append-only checksum manifest.
package migrations

import (
	"embed"
	"io/fs"
)

//go:embed *.sql
var files embed.FS

// FS is the embedded migration tree.
func FS() fs.FS { return files }
