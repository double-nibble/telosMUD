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
# The root go.mod `replace`s github.com/yuin/gopher-lua with the in-tree vendored fork
# (./third_party/gopher-lua), so `go mod download` must see the fork's go.mod — copy it
# (the whole tree; it's small + changes rarely, so the dep layer still caches well) BEFORE
# the download. Without this the build fails: "reading third_party/gopher-lua/go.mod: no
# such file or directory" (the fork was vendored in slice 7.1).
COPY third_party ./third_party
RUN go mod download
COPY . .
COPY --from=proto /work/api/gen ./api/gen
ARG SERVICE
RUN test -n "$SERVICE" && go build -trimpath -ldflags="-s -w" -o /out/app ./cmd/${SERVICE}

# Minimal, non-root runtime. The binary is static (CGO disabled).
FROM gcr.io/distroless/static-debian12:nonroot AS runtime
COPY --from=build /out/app /usr/local/bin/app
ENTRYPOINT ["/usr/local/bin/app"]
