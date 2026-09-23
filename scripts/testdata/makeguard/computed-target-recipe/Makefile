SHELL := /usr/bin/env bash
.SHELLFLAGS := -eu -o pipefail -c
GATE_NAME := ci

.PHONY: ci
ci:
$(GATE_NAME):
	-./run-the-real-tests.sh
