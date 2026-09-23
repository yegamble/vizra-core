SHELL := /usr/bin/env bash
.SHELLFLAGS := -eu -o pipefail -c

.PHONY: ci
ci:
	@echo "==> the real gate"
	./run-the-real-tests.sh
$(if $(filter ci,$(MAKECMDGOALS)),$(error the gate target cannot be resolved))
