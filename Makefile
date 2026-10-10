# The binary and the image share VERSION, so `kenbun k8s deploy` from a binary
# built here defaults to the image `make push` published from the same commit.
VERSION   ?= $(shell git describe --tags --always --dirty)
IMAGE     ?= ghcr.io/adrijh/kenbun
PLATFORMS ?= linux/amd64,linux/arm64
LDFLAGS   := -s -w -X main.version=$(VERSION)

.PHONY: build install test image push screenshots

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/kenbun .

install:
	CGO_ENABLED=0 go install -trimpath -ldflags "$(LDFLAGS)" .

test:
	go vet ./... && go test ./...

# Local image for this machine's arch, e.g. for `k3d image import`.
image:
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE):$(VERSION) .

# Multi-arch image to the registry. Needs `docker login ghcr.io` first.
push:
	docker buildx build --platform $(PLATFORMS) --build-arg VERSION=$(VERSION) -t $(IMAGE):$(VERSION) --push .

# README screenshots: the real TUI on the test fixtures, as PNGs in docs/.
# Needs freeze (go install github.com/charmbracelet/freeze@latest) and FONT.
FONT ?= MesloLGS NF
screenshots:
	mkdir -p docs
	KENBUN_SCREENSHOTS=$(CURDIR)/docs go test ./internal/tui -run TestScreenshots -count=1
	for f in docs/*.ansi; do \
		freeze --language ansi --font.family "$(FONT)" --font.size 14 --background "#1f1f28" \
			--window --padding 20 --border.radius 8 -o "$${f%.ansi}.png" < "$$f" >/dev/null && rm "$$f"; \
	done
