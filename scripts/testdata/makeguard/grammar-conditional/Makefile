SHELL := /usr/bin/env bash
.SHELLFLAGS := -eu -o pipefail -c

.PHONY: ci
ci:
	@echo "==> the real gate"
	./run-the-real-tests.sh

ifdef VIZRA_B5D_NEVER_SET
INERT_CONDITIONAL := 1
endif
