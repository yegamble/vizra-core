SHELL := /usr/bin/env bash
.SHELLFLAGS := -eu -o pipefail -c

.PHONY: ci
ci:
	@echo "==> the real gate"
	./run-the-real-tests.sh
PREFIX_HEAD := .RECIPE
$(PREFIX_HEAD)PREFIX := >
