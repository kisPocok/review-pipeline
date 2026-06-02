HOOK_BIN := hook/bin/pre-commit-check
HOOK_SRC := ./hook/cmd/pre-commit-check

.PHONY: build test clean

build: $(HOOK_BIN)

$(HOOK_BIN): $(shell find hook -name '*.go') go.mod go.sum
	go build -o $(HOOK_BIN) $(HOOK_SRC)

test:
	go test ./... -count=1

clean:
	rm -f $(HOOK_BIN)
