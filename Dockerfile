FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
# Pure Go (SQLite is modernc), so cross-compiling needs no C toolchain or QEMU.
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/animeworldtv-indexer ./cmd/animeworldtv-indexer

FROM alpine:3.22
LABEL org.opencontainers.image.source="https://github.com/easly1989/animeworldtv-indexer"
LABEL org.opencontainers.image.description="Torznab indexer for AnimeWorld"
RUN apk add --no-cache ca-certificates tzdata
COPY --from=build /out/animeworldtv-indexer /usr/local/bin/animeworldtv-indexer

ENV PROXY_HOST=0.0.0.0 \
    PROXY_PORT=9698 \
    DATA_DIR=/app/data \
    LOG_LEVEL=INFO
VOLUME ["/app/data"]
EXPOSE 9698

# busybox wget ships with alpine; no curl needed.
HEALTHCHECK --interval=30s --timeout=10s --start-period=5s --retries=3 \
    CMD wget -q -O /dev/null http://127.0.0.1:${PROXY_PORT}/health || exit 1

CMD ["animeworldtv-indexer"]
