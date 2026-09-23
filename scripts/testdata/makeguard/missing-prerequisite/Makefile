SHELL := /usr/bin/env bash
.SHELLFLAGS := -eu -o pipefail -c

.PHONY: ci
ci: no-such-file-and-no-rule
	@echo "==> the real gate"
	./run-the-real-tests.sh
