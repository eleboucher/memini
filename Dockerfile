# syntax=docker/dockerfile:1
ARG GO_VERSION=1.26.4

# --- build stage ---------------------------------------------------------
FROM golang:${GO_VERSION}-alpine AS build
WORKDIR /workspace

# Cache modules separately from source.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG REVISION=none
ARG DATE=unknown

RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build \
    -ldflags "-s -w \
      -X github.com/eleboucher/memini/internal/version.Version=${VERSION} \
      -X github.com/eleboucher/memini/internal/version.Commit=${REVISION} \
      -X github.com/eleboucher/memini/internal/version.Date=${DATE}" \
    -o /out/memini ./cmd/memini

# --- runtime stage -------------------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/memini /usr/local/bin/memini
EXPOSE 8080
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/memini"]
