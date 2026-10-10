# Static, pure-Go build (no cgo: modernc SQLite) on distroless. Cross-compiles
# on the build host, so multi-arch images need no emulation.
FROM --platform=$BUILDPLATFORM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
ARG TARGETOS TARGETARCH VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X main.version=$VERSION" -o /kenbun .

FROM gcr.io/distroless/static-debian12:nonroot
# Links the ghcr.io package to the repo, so it shows under its Packages.
LABEL org.opencontainers.image.source=https://github.com/adrijh/kenbun
COPY --from=build /kenbun /kenbun
EXPOSE 4317 7070
ENTRYPOINT ["/kenbun"]
CMD ["serve"]
