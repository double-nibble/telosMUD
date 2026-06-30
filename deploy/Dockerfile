# syntax=docker/dockerfile:1

# Generate protobuf/gRPC code. Generated *.pb.go is gitignored, so the image
# (re)generates it here rather than relying on the build context to carry it.
FROM bufbuild/buf:1.71.0 AS proto
WORKDIR /work
COPY buf.yaml buf.gen.yaml ./
COPY api/proto ./api/proto
RUN ["buf", "generate"]

# Build one service binary, selected by the SERVICE build arg (telos-gate / telos-world).
FROM golang:1.25-alpine AS build
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
RUN test -n "$SERVICE" && go build -trimpath -ldflags="-s -w" -o /out/app ./cmd/${SERVICE}

# Minimal, non-root runtime. The binary is static (CGO disabled).
FROM gcr.io/distroless/static-debian12:nonroot AS runtime
COPY --from=build /out/app /usr/local/bin/app
ENTRYPOINT ["/usr/local/bin/app"]
