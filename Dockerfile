# Build stage
FROM golang:1.27.4-trixie@sha256:f18c3d73a184fb0582828510722e723b63a93f7188d11dd223d9be6f6880e65d AS builder
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
FROM gcr.io/distroless/static-debian13:nonroot@sha256:8ce6dd87e9823c5b48dcfb2c8604714ccb351ce59607327bcc4957448da279b5
WORKDIR /
COPY --from=builder /bin/app /app
USER 65532:65532
ENTRYPOINT ["/app"]
