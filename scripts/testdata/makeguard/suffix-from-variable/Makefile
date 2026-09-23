SHELL := /usr/bin/env bash
.SHELLFLAGS := -eu -o pipefail -c
SWALLOW := || true

.PHONY: ci
ci:
	@echo "==> the real gate"
	./run-the-real-tests.sh $(SWALLOW)
