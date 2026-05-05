BIN          := goon
PKGS         := ./...

# Install destinations
PREFIX       ?= /usr/local
SYSTEM_DIR   := $(PREFIX)/bin
USER_DIR     := $(HOME)/.local/bin
GO_BIN       := $(shell go env GOBIN 2>/dev/null)
ifeq ($(GO_BIN),)
GO_BIN       := $(shell go env GOPATH)/bin
endif

.PHONY: all build test vet fmt clean tidy check \
        run run-auto run-explain \
        install install-system install-user install-go \
        uninstall uninstall-system uninstall-user uninstall-go

all: check build

build:
	go build -trimpath -ldflags='-s -w' -o $(BIN) .

# --- Run on the fly (no build, no install) ----------------------------------
# Pass the task in the TASK variable. Examples:
#   make run TASK='list every .go file under internal'
#   make run-auto TASK='tidy go.mod'
#   make run-explain TASK='delete every .log older than 30 days'
run:
	@go run . $(if $(TASK),"$(TASK)",) $(ARGS)

run-auto:
	@go run . $(if $(TASK),"$(TASK)",) --auto $(ARGS)

run-explain:
	@go run . $(if $(TASK),"$(TASK)",) --explain $(ARGS)

test:
	go test -race -count=1 $(PKGS)

vet:
	go vet $(PKGS)

fmt:
	gofmt -s -w .

tidy:
	go mod tidy

check: vet test

clean:
	rm -f $(BIN)

# --- Install -----------------------------------------------------------------
# Default: user-local install (no sudo required, works on macOS + Linux).
install: install-user

# 1. User-local install: copies the binary to ~/.local/bin/goon.
#    Make sure ~/.local/bin is on your PATH (most modern shells do this for you,
#    otherwise add: export PATH="$$HOME/.local/bin:$$PATH").
install-user: build
	@mkdir -p $(USER_DIR)
	@install -m 0755 $(BIN) $(USER_DIR)/$(BIN)
	@echo "installed: $(USER_DIR)/$(BIN)"
	@case ":$$PATH:" in *":$(USER_DIR):"*) ;; \
	  *) echo "warning: $(USER_DIR) is not on your PATH"; \
	     echo "         add this to your shell rc:  export PATH=\"$(USER_DIR):\$$PATH\"" ;; \
	esac

# 2. System-wide install: requires sudo on most systems.
install-system: build
	@install -d $(SYSTEM_DIR)
	@install -m 0755 $(BIN) $(SYSTEM_DIR)/$(BIN)
	@echo "installed: $(SYSTEM_DIR)/$(BIN)"

# 3. Go install: builds + copies to $GOBIN (or $GOPATH/bin).
install-go:
	go install -trimpath -ldflags='-s -w' .
	@echo "installed: $(GO_BIN)/$(BIN)"
	@case ":$$PATH:" in *":$(GO_BIN):"*) ;; \
	  *) echo "warning: $(GO_BIN) is not on your PATH"; \
	     echo "         add this to your shell rc:  export PATH=\"$(GO_BIN):\$$PATH\"" ;; \
	esac

# --- Uninstall ---------------------------------------------------------------
uninstall: uninstall-user

uninstall-user:
	@rm -f $(USER_DIR)/$(BIN) && echo "removed: $(USER_DIR)/$(BIN)"

uninstall-system:
	@rm -f $(SYSTEM_DIR)/$(BIN) && echo "removed: $(SYSTEM_DIR)/$(BIN)"

uninstall-go:
	@rm -f $(GO_BIN)/$(BIN) && echo "removed: $(GO_BIN)/$(BIN)"

# --- Self-update via the running binary -------------------------------------
update:
	$(BIN) update

# --- Quick demo runs (require .env or env vars) ---
demo-dry:
	./$(BIN) "list every .go file under internal"

demo-run:
	./$(BIN) "list every .go file under internal" --run

demo-auto:
	./$(BIN) "list every .go file under internal" --auto

demo-explain:
	./$(BIN) "delete every .log older than 30 days" --explain
