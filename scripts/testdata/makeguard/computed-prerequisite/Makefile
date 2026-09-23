SHELL := /usr/bin/env bash
.SHELLFLAGS := -eu -o pipefail -c
LANE := lane

.PHONY: ci lane
ci: $(LANE)
	@echo "==> the real gate"
lane:
	./run-the-real-tests.sh
