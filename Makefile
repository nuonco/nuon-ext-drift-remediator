BINARY      := nuon-ext-drift-remediator
BUILD_DIR   := .
GO          ?= go
INSTALL_DIR ?= $(HOME)/.config/nuon/extensions/$(BINARY)

.DEFAULT_GOAL := build

.PHONY: all build test fmt vet lint tidy clean install

all: fmt vet test build

build:
	$(GO) build -o $(BUILD_DIR)/$(BINARY) .

test:
	$(GO) test ./...

fmt:
	$(GO) fmt ./...

vet:
	$(GO) vet ./...

lint:
	@command -v golangci-lint >/dev/null 2>&1 \
		&& golangci-lint run ./... \
		|| echo "golangci-lint not installed; skipping lint"

tidy:
	$(GO) mod tidy

clean:
	rm -f $(BUILD_DIR)/$(BINARY)

install: build
	mkdir -p $(INSTALL_DIR)
	cp $(BUILD_DIR)/$(BINARY) $(INSTALL_DIR)/$(BINARY)
