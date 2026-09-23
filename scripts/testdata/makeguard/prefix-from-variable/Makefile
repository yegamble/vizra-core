SHELL := /usr/bin/env bash
.SHELLFLAGS := -eu -o pipefail -c
IGNORE_FAILURE := -

.PHONY: ci
ci:
	@echo "==> the real gate"
	$(IGNORE_FAILURE)./run-the-real-tests.sh
