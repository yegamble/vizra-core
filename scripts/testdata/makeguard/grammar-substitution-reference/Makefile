SHELL := /usr/bin/env bash
.SHELLFLAGS := -eu -o pipefail -c

.PHONY: ci
ci:
	@echo "==> the real gate"
	./run-the-real-tests.sh

INERT_LIST := a
INERT_SUBST := $(INERT_LIST:a=b)
