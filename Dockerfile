# Build stage
FROM golang:1.27.3-trixie@sha256:fbb7ae40b62ff8afe31662df570d39b4f5052ac32b90dae8e60d7657a20a74f4 AS builder
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
# The main package the image runs. Left empty, the build takes the module's only main
# package, wherever it lives, and stops naming what it found when there is none or more than
# one. Choose one with: docker build --build-arg MAIN_PACKAGE=./cmd/<name> .
ARG MAIN_PACKAGE=./cmd/epitropctl
RUN set -eu; \
    pkg="${MAIN_PACKAGE:-}"; \
    if [ -z "$pkg" ]; then \
      listed="$(go list -f '{{if eq .Name "main"}}{{.ImportPath}}{{end}}' ./...)"; \
      mains=0; names=""; \
      for candidate in $listed; do mains=$((mains + 1)); pkg="$candidate"; names="$names $candidate"; done; \
      if [ "$mains" -ne 1 ]; then \
        echo "MAIN_PACKAGE is unset and the module has $mains main packages:${names:- none}" >&2; \
        exit 1; \
      fi; \
    fi; \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /bin/app "$pkg"

# Distroless runtime
FROM gcr.io/distroless/static-debian13:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3
WORKDIR /
COPY --from=builder /bin/app /app
USER 65532:65532
ENTRYPOINT ["/app"]
