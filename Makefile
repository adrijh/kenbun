# The binary and the image share VERSION, so `kenbun k8s deploy` from a binary
# built here defaults to the image `make push` published from the same commit.
VERSION   ?= $(shell git describe --tags --always --dirty)
IMAGE     ?= ghcr.io/adrijh/kenbun
PLATFORMS ?= linux/amd64,linux/arm64
DIST      ?= linux/amd64 linux/arm64 darwin/amd64 darwin/arm64
LDFLAGS   := -s -w -X main.version=$(VERSION)

.PHONY: build install test image push dist screenshots

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
# Refuses -dirty versions: re-pushing one tag isn't pulled by nodes that have it.
push:
	@case "$(VERSION)" in *-dirty) echo "push: uncommitted changes; commit first ($(VERSION))" >&2; exit 1;; esac
	docker buildx build --platform $(PLATFORMS) --build-arg VERSION=$(VERSION) -t $(IMAGE):$(VERSION) --push .

# Release archives in dist/, one per DIST platform. Names carry no version, so
# releases/latest/download/kenbun_linux_amd64.tar.gz always works.
dist:
	rm -rf dist && mkdir dist
	for p in $(DIST); do \
		os=$${p%/*}; arch=$${p#*/}; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "$(LDFLAGS)" -o dist/kenbun . && \
		tar -czf dist/kenbun_$${os}_$${arch}.tar.gz -C dist kenbun && rm dist/kenbun || exit 1; \
	done
	cd dist && sha256sum *.tar.gz > checksums.txt

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
