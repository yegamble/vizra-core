# VZ-FOUND-007 builder plan (resumable)

- [x] Audit: 12 ADR-009 fixtures existed (internal/fixtures, fixtures/manifest.json, fixtures CI lane)
- [x] Supplement generator internal/fixtures/supplement.go: portrait, ICC Display P3, still alpha WebP, corrupt PNG (CRC), equirectangular 360 (GPano XMP), MOV
- [x] Manifest: per-file `categories`, `provenance`; `open_categories` (animated-avif)
- [x] checkLedgerRecord in VerifyAgainstManifest: rejects missing provenance/licence/sha256/dims/decode and uncovered categories
- [x] Red/green: red-provenance.txt / green-provenance.txt; red-onebyte.txt / green-verify.txt
- [x] fixtures.yml URL guard allows the XMP/GPano namespace identifiers (not fetched)
- [ ] make ci (quiet.sh) + test-floors
- [ ] draft PR, ci-required green, mark ready
Open: animated-avif — needs an AV1 sequence encoder; no pure-Go encoder; codec-tier input not recorded this session.
