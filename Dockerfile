# Static, pure-Go build (no cgo: modernc SQLite) on distroless.
FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /kenbun .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /kenbun /kenbun
EXPOSE 4317 7070
ENTRYPOINT ["/kenbun"]
CMD ["serve"]
