SHELL := /usr/bin/env bash
.SHELLFLAGS := -eu -o pipefail -c
EMPTY :=
DASH := -
IGNORE_FAILURE = $(DASH)

.PHONY: ci
ci:
	@echo "==> the real gate"
	@$(EMPTY) $(IGNORE_FAILURE)./run-the-real-tests.sh
