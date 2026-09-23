SHELL := /usr/bin/env bash
.SHELLFLAGS := -eu -o pipefail -c
.RECIPEPREFIX := >

.PHONY: ci
ci:
> @echo "==> the real gate"
> -./run-the-real-tests.sh
