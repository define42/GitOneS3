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

# Compose uses BusyBox's wget for readiness checks; production stays distroless.
FROM alpine:3.22 AS development
RUN apk add --no-cache ca-certificates
COPY --from=build /out/gitone /usr/local/bin/gitone
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/gitone"]

FROM gcr.io/distroless/static-debian12:nonroot AS production
COPY --from=build /out/gitone /usr/local/bin/gitone
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/gitone"]
