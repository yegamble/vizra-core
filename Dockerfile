# vizra-core release image (ADR-001 § Image and video).
#
# Layer order is load-bearing, not cosmetic: the libvips layer sits BELOW the Go
# binary layer, so a Go change never rebuilds libvips and the codec surface of a
# tag is stable across rebuilds. Two builds of one commit must resolve identical
# library versions, and the versions each build resolved are recorded beside the
# loader list.
#
# Every FROM carries an @sha256 digest. A floating tag would let the base change
# under a tag that is supposed to be reproducible, which is the whole point of
# recording a loader list.
#
# NOT here, by decision:
#   * x265 — never in any image (ADR-001). GPL-2+, and the loader-list assertion
#     below is the detector if a transitive dependency ever pulls it in.
#   * libheif/libde265 — HEIC decode exists only in the HEIC option image, which
#     is a separate build flag and a separate licence review (VZ-MEDIA-010, M3).
#   * ffmpeg/ffprobe — video poster and probe only, added with VZ-MEDIA-005 (M3).

# --- pinned inputs -----------------------------------------------------------
# golang:1.27.1-trixie, digest resolved from registry-1.docker.io on 2026-09-20.
FROM golang@sha256:433790e515d27dc6003e847e644cc0af956985cf315c1c58a3b73ee2dd305183 AS gobuild

# debian:13-slim, digest resolved from registry-1.docker.io on 2026-09-20.
FROM debian@sha256:a99cfc517144bc59b1978475ec53b46ecabec7e43635402ee5b77cc54cd1b20a AS vips

# libvips 8.18.6, from a CHECKSUMMED source tarball: distribution packages
# cannot pin 8.18, and a derivative hash that is not reproducible is not
# evidence. sha256 published by upstream at
# https://github.com/libvips/libvips/releases/download/v8.18.6/vips-8.18.6.tar.xz.sha256sum
ARG VIPS_VERSION=8.18.6
ARG VIPS_SHA256=3c41e1d5458081bfa4a5bc54e116c46259c75c6760a18027764555632b9dda3e

ENV DEBIAN_FRONTEND=noninteractive
RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends \
        ca-certificates curl xz-utils pkg-config \
        meson ninja-build build-essential \
        libglib2.0-dev libexpat1-dev \
        libjpeg62-turbo-dev libpng-dev libwebp-dev libtiff-dev \
        libexif-dev liblcms2-dev libspng-dev \
        libdav1d-dev libaom-dev; \
    rm -rf /var/lib/apt/lists/*

WORKDIR /src
RUN set -eux; \
    curl -fsSLo vips.tar.xz \
      "https://github.com/libvips/libvips/releases/download/v${VIPS_VERSION}/vips-${VIPS_VERSION}.tar.xz"; \
    echo "${VIPS_SHA256}  vips.tar.xz" | sha256sum -c -; \
    tar -xf vips.tar.xz; \
    cd "vips-${VIPS_VERSION}"; \
    meson setup build --prefix=/usr/local --buildtype=release \
        -Ddeprecated=false -Dexamples=false -Dcplusplus=false \
        -Dintrospection=disabled -Dmodules=disabled; \
    meson compile -C build; \
    meson install -C build; \
    ldconfig

# Record what this build actually produced. `vips -l` is the ONLY detector for a
# codec that entered the image silently, so it is captured here and served by
# /version rather than only logged.
RUN set -eux; \
    mkdir -p /out; \
    vips --version > /out/libvips-version.txt; \
    vips -l > /out/libvips-loaders-full.txt; \
    vips -l | sed -n 's/^[[:space:]]*Vips[A-Za-z0-9]*[[:space:]]*(\([a-z0-9_]*\)).*/\1/p' \
      | grep -E '(load|save)' | sort -u | paste -sd, - > /out/libvips-loaders.txt; \
    test -s /out/libvips-loaders.txt; \
    dpkg-query -W -f='${binary:Package}=${Version}\n' > /out/apt-versions.txt; \
    echo "--- loaders ---"; cat /out/libvips-loaders.txt; \
    if vips -l | grep -qi 'x265\|heif\|heic'; then \
      echo "REFUSED: an excluded codec is present in the loader list" >&2; exit 1; \
    fi

# --- Go build ----------------------------------------------------------------
# This stage is BELOW libvips in build order and depends on nothing from it, so
# editing Go code reuses the libvips layers.
FROM gobuild AS build
WORKDIR /src

ARG RELEASE=dev
ARG COMMIT=unknown
ARG BUILT_AT=

COPY go.mod go.sum ./
RUN go mod download

COPY . .
COPY --from=vips /out/libvips-version.txt /out/libvips-loaders.txt /buildmeta/

# CGO is off: cmd/api and cmd/vizra never decode pixels (ADR-002, Q-034), so they
# need no libvips binding at all. cmd/worker gains govips with VZ-MEDIA-001 in
# M1, and gets its own CGO-enabled stage then.
RUN set -eux; \
    LIBVIPS_VERSION="$(cat /buildmeta/libvips-version.txt | tr -d '\n')"; \
    LIBVIPS_LOADERS="$(cat /buildmeta/libvips-loaders.txt | tr -d '\n')"; \
    LDFLAGS="-s -w \
      -X github.com/yegamble/vizra-core/internal/buildinfo.Release=${RELEASE} \
      -X github.com/yegamble/vizra-core/internal/buildinfo.Commit=${COMMIT} \
      -X github.com/yegamble/vizra-core/internal/buildinfo.BuiltAt=${BUILT_AT} \
      -X github.com/yegamble/vizra-core/internal/buildinfo.LibvipsVersion=${LIBVIPS_VERSION} \
      -X github.com/yegamble/vizra-core/internal/buildinfo.libvipsLoaders=${LIBVIPS_LOADERS}"; \
    CGO_ENABLED=0 go build -trimpath -ldflags "${LDFLAGS}" -o /out/vizra-api    ./cmd/api; \
    CGO_ENABLED=0 go build -trimpath -ldflags "${LDFLAGS}" -o /out/vizra-worker ./cmd/worker; \
    CGO_ENABLED=0 go build -trimpath -ldflags "${LDFLAGS}" -o /out/vizra        ./cmd/vizra

# --- runtime -----------------------------------------------------------------
FROM vips AS runtime

# Runtime needs the shared libraries, not the toolchain.
RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends ca-certificates tini; \
    apt-get purge -y --auto-remove meson ninja-build build-essential pkg-config curl xz-utils || true; \
    rm -rf /var/lib/apt/lists/* /src

# Licence obligations travel with the image (ADR-001 § Licence table;
# DEFINITION_OF_DONE requires the notices preserved). libvips is LGPL-2.1+, so
# its notice, its text and the source offer ship here.
COPY --from=vips /out/libvips-version.txt /out/libvips-loaders-full.txt /out/apt-versions.txt /usr/share/vizra/build/
COPY LICENSE /usr/share/vizra/licenses/vizra-core.LICENSE
COPY NOTICE   /usr/share/vizra/licenses/NOTICE

COPY --from=build /out/vizra-api    /usr/local/bin/vizra-api
COPY --from=build /out/vizra-worker /usr/local/bin/vizra-worker
COPY --from=build /out/vizra        /usr/local/bin/vizra

# Never root. The api writes nothing to the filesystem.
RUN set -eux; \
    groupadd --system --gid 10001 vizra; \
    useradd --system --uid 10001 --gid vizra --home /var/lib/vizra --create-home vizra
USER 10001:10001

ENV VIZRA_LISTEN_ADDR=:8080 \
    VIZRA_METRICS_ADDR=127.0.0.1:9090
EXPOSE 8080

# The healthcheck uses the binary that is already in the image rather than
# adding curl to the runtime surface.
HEALTHCHECK --interval=15s --timeout=3s --start-period=10s --retries=3 \
    CMD ["/usr/local/bin/vizra", "version"]

ENTRYPOINT ["/usr/bin/tini", "--"]
CMD ["/usr/local/bin/vizra-api"]
