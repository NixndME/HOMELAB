# syntax=docker/dockerfile:1
# Multi-arch build: the Go compiler cross-compiles, so arm64 images build fast on amd64 (no QEMU).
FROM --platform=$BUILDPLATFORM golang:1-alpine AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/cp ./cmd/cp

# distroless/static: no shell, no package manager, runs as non-root by default.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/cp /cp
ENV CP_ADDR=:8080 CP_DATA_DIR=/data
VOLUME /data
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 CMD ["/cp", "healthcheck"]
ENTRYPOINT ["/cp"]
