# syntax=docker/dockerfile:1

# Build one service binary, selected by the SERVICE build arg (telos-gate / telos-world).
FROM golang:1.25-alpine AS build
WORKDIR /src
ENV GOWORK=off CGO_ENABLED=0
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG SERVICE
RUN test -n "$SERVICE" && go build -trimpath -ldflags="-s -w" -o /out/app ./cmd/${SERVICE}

# Minimal, non-root runtime. The binary is static (CGO disabled).
FROM gcr.io/distroless/static-debian12:nonroot AS runtime
COPY --from=build /out/app /usr/local/bin/app
ENTRYPOINT ["/usr/local/bin/app"]
