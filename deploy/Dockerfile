# syntax=docker/dockerfile:1

# Generate protobuf/gRPC code. Generated *.pb.go is gitignored, so the image
# (re)generates it here rather than relying on the build context to carry it.
# Pinned to the BUILD platform: codegen output is arch-independent, so never emulate it.
FROM --platform=$BUILDPLATFORM bufbuild/buf:1.71.0 AS proto
WORKDIR /work
COPY buf.yaml buf.gen.yaml ./
COPY api/proto ./api/proto
RUN ["buf", "generate"]

# Build one service binary, selected by the SERVICE build arg (telos-gate / telos-world).
# Runs on the BUILD platform and cross-compiles to TARGETARCH (set automatically by buildx),
# so multi-arch (amd64 + arm64) builds stay native-fast instead of running the toolchain
# under QEMU. The output binary is static (CGO disabled), so cross-compiling is free.
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS build
WORKDIR /src
ENV GOWORK=off CGO_ENABLED=0
COPY go.mod go.sum ./
# The root go.mod `replace`s github.com/yuin/gopher-lua with our remote fork
# (github.com/double-nibble/gopher-lua), so `go mod download` fetches it from the module proxy
# like any other dependency — no in-tree vendoring to COPY anymore.
RUN go mod download
COPY . .
COPY --from=proto /work/api/gen ./api/gen
ARG SERVICE
# BUILD_TAGS selects optional build tags. The default (empty) build is the RELEASE build: the
# TELOS_DEV_AUTOAUTH OAuth bypass is compiled OUT of telos-gate (#96). The dev/smoke/e2e stack sets
# BUILD_TAGS=telos_devauth on the gate service (deploy/docker-compose.yml) so its headless bare-name login
# works; a production image builds with no tags and cannot enable the bypass by any env/config.
ARG BUILD_TAGS=""
# TARGETOS/TARGETARCH are provided by buildx from the requested --platform (#263 multi-arch cross-compile).
ARG TARGETOS
ARG TARGETARCH
RUN test -n "$SERVICE" && GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -tags "${BUILD_TAGS}" -ldflags="-s -w" -o /out/app ./cmd/${SERVICE}

# Minimal, non-root runtime. The binary is static (CGO disabled).
FROM gcr.io/distroless/static-debian12:nonroot AS runtime
COPY --from=build /out/app /usr/local/bin/app
ENTRYPOINT ["/usr/local/bin/app"]
