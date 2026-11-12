# VZ-TOPOLOGY-007 core part — resumable checklist

Config key in core is `VIZRA_MODE` (development|production), not `VIZRA_ENV`.

## Seam table (internal/config)
| Seam | Exists in core? | Production refusal | Test |
|---|---|---|---|
| Rate limit off | NO — internal/cache/ratelimit.go has no disable switch | n/a (not invented) | — |
| Mail capture | NO — no mail/SMTP config in core | n/a (not invented) | — |
| Plain-http origin | VIZRA_PUBLIC_ORIGIN http | refused unless VIZRA_ALLOW_INSECURE_PUBLIC_ORIGIN=true | TestProductionRefusesPlainHTTPOriginUnlessExplicitlyAllowed |
| Plain-http/wildcard CORS | VIZRA_CORS_ALLOWED_ORIGINS | refused | TestProductionRefusesWildcardCORS / PlainHTTPCORSOrigin |
| Dev secrets | session/KEK/search HMAC | refused (known dev, published, short) | TestProductionRefusesDevSecrets etc. |
| VIZRA_DEV_DISABLE_AUTH, _ALLOW_ANY_ORIGIN, _SKIP_MIGRATIONS, _FAKE_SEARCH, _INSECURE_COOKIES, _TRUST_ANY_HMAC | yes (EscapeHatches) | refused when truthy, named error | TestEveryEscapeHatchIsRefusedInProduction (table over registry) |
| VIZRA_DEV_AUTOLOGIN_USER | yes | refused on presence | TestValueBearingEscapeHatchIsRefusedWhenPresent |

Every existing seam was already refused (core #25); table test enumerates the registry, so a new hatch is covered automatically. Mutation M3 in redgreen.txt.

## Negative: override never auto-loaded
- [x] setup: exhaustive answer walk + supportedShapes never contain override/dev file (TestProductionChainNeverIncludesOverride; M1)
- [x] ops: every Argv has one -f per chain entry (TestArgvNeverReliesOnAutoLoad; M2)
- [x] doctor topology: was `docker compose --env-file X config` with COMPOSE_FILE via env; now explicit -f via ops.LoadChain (TestDoctorTopologyArgvHasExplicitFiles; red = build failure before fix)
- `compose version --short` (cmd/vizra/ops.go, doctor.go) loads no project — exempt.
- [ ] make ci via quiet.sh
- [ ] ci-required green -> ready
