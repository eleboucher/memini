# syntax=docker/dockerfile:1
ARG RUST_VERSION=1.96
ARG NODE_VERSION=24

FROM --platform=$BUILDPLATFORM node:${NODE_VERSION}-bookworm-slim AS ui
WORKDIR /ui
COPY ui/package.json ui/package-lock.json ./
RUN --mount=type=cache,target=/root/.npm npm ci
COPY ui/ ./
RUN npm run build

FROM rust:${RUST_VERSION}-bookworm AS build
WORKDIR /workspace

COPY Cargo.toml Cargo.lock ./
COPY crates ./crates
COPY ui/dist ./ui/dist

COPY --from=ui /ui/dist ./ui/dist

ARG TARGETARCH
ARG VERSION=dev
ARG REVISION=none
ARG DATE=unknown

RUN --mount=type=cache,target=/usr/local/cargo/registry \
    --mount=type=cache,target=/usr/local/cargo/git \
    --mount=type=cache,target=/workspace/target,id=cargo-${TARGETARCH} \
    mkdir -p /out && \
    MEMINI_BUILD_VERSION=${VERSION} MEMINI_BUILD_REVISION=${REVISION} MEMINI_BUILD_DATE=${DATE} \
    cargo build --locked --release -p memini-cli && \
    cp target/release/memini /out/memini

# Bare binary for `--target artifact --output type=local`; the release
# workflow extracts these instead of recompiling for the archives.
FROM scratch AS artifact
COPY --from=build /out/memini /memini

FROM gcr.io/distroless/cc-debian13:debug-nonroot
LABEL org.opencontainers.image.source="https://git.erwanleboucher.dev/eleboucher/memini"
LABEL org.opencontainers.image.url="https://git.erwanleboucher.dev/eleboucher/memini"
LABEL org.opencontainers.image.documentation="https://git.erwanleboucher.dev/eleboucher/memini"
LABEL org.opencontainers.image.licenses="MIT"
COPY --from=build /out/memini /usr/local/bin/memini
EXPOSE 8080
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/memini"]
