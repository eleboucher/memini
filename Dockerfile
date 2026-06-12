# syntax=docker/dockerfile:1
ARG GO_VERSION=1.26.4
ARG NODE_VERSION=24

FROM --platform=$BUILDPLATFORM node:${NODE_VERSION}-alpine AS ui
WORKDIR /ui
COPY ui/package.json ui/package-lock.json ./
RUN --mount=type=cache,target=/root/.npm npm ci
COPY ui/ ./
RUN npm run build

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS build
WORKDIR /workspace

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .
COPY --from=ui /internal/api/ui/dist ./internal/api/ui/dist

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG REVISION=none
ARG DATE=unknown

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build,id=gobuild-${TARGETARCH} \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build \
    -ldflags "-s -w \
      -X github.com/eleboucher/memini/internal/version.Version=${VERSION} \
      -X github.com/eleboucher/memini/internal/version.Commit=${REVISION} \
      -X github.com/eleboucher/memini/internal/version.Date=${DATE}" \
    -o /out/memini ./cmd/memini

# Bare binary for `--target artifact --output type=local`; the release
# workflow extracts these instead of recompiling for the archives.
FROM scratch AS artifact
COPY --from=build /out/memini /memini

FROM gcr.io/distroless/static-debian13:debug-nonroot
COPY --from=build /out/memini /usr/local/bin/memini
EXPOSE 8080
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/memini"]
