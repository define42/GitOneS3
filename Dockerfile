# syntax=docker/dockerfile:1.7
FROM golang:1.24-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .
ARG VERSION=dev
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build \
    -trimpath \
    -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/gitone ./cmd/gitone

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/gitone /usr/local/bin/gitone
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/gitone"]
