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

# --- what the runtime actually needs ------------------------------------------
# The runtime stage is built from a CLEAN base (see below), so it must install
# the shared libraries libvips links against. That list is DERIVED here rather
# than typed by hand: a hand-typed list is a second source of truth that drifts
# silently the day libvips gains or drops a dependency, and the symptom is a
# runtime image whose `vips` cannot load a format the loader list promises.
#
# This stage exists separately from `vips` so the `vips` stage's bytes do not
# move when this derivation changes — the libvips compile is the expensive layer
# and it stays cacheable.
FROM vips AS vipsmeta
RUN set -eux; \
    mkdir -p /out; \
    find /usr/local/lib -name 'libvips*.so*' -type f > /tmp/vipslibs; \
    test -s /tmp/vipslibs; \
    { xargs ldd < /tmp/vipslibs; ldd /usr/local/bin/vips; } \
      | awk '$3 ~ /^\// {print $3}' | sort -u > /tmp/deps; \
    test -s /tmp/deps; \
    grep -v '^/usr/local/' /tmp/deps > /tmp/systemdeps; \
    test -s /tmp/systemdeps; \
    xargs readlink -f < /tmp/systemdeps | sort -u > /tmp/real; \
    xargs dpkg-query -S < /tmp/real \
      | sed 's/:[^:]*$//' | tr ',' '\n' | sed 's/:[a-z0-9-]*$//' \
      | tr -d ' ' | sort -u > /out/runtime-packages.txt; \
    test -s /out/runtime-packages.txt; \
    echo "--- runtime packages ---"; cat /out/runtime-packages.txt

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
# FROM A CLEAN BASE, not `FROM vips`.
#
# It used to be `FROM vips`, i.e. the image that compiled libvips, with the
# toolchain purged afterwards. Two things were wrong with that, and the second
# is the one that matters:
#
#  1. `apt-get purge --auto-remove … || true` cannot fail. A purge that stops
#     working — a renamed package, a dependency that pins one of them — leaves
#     meson, ninja, gcc, curl and the whole -dev set in the SHIPPING image, and
#     the build stays green. There is no `|| true` anywhere in this file now,
#     and there is nothing left to purge: a clean base never had a toolchain.
#  2. Even a purge that works leaves the layers it removed FROM. A deleted file
#     in a later layer still ships its bytes in the earlier one, so `docker save`
#     and every registry copy carried the compiler regardless. Starting from a
#     clean base is the only way that is actually not there.
#
# What is carried across is exactly: the libvips install tree, the three Go
# binaries, and the licence/build metadata. The shared libraries libvips needs
# are installed from the distribution, from the list the vipsmeta stage derived
# with ldd + dpkg-query.
#
# debian:13-slim, the SAME digest the vips stage uses — so the two stages agree
# on the distribution the libraries came from, which is the assumption ldd's
# answer is only valid under.
FROM debian@sha256:a99cfc517144bc59b1978475ec53b46ecabec7e43635402ee5b77cc54cd1b20a AS runtime

ENV DEBIAN_FRONTEND=noninteractive
COPY --from=vipsmeta /out/runtime-packages.txt /tmp/runtime-packages.txt
RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends \
        ca-certificates tini \
        $(tr '\n' ' ' < /tmp/runtime-packages.txt); \
    rm -rf /var/lib/apt/lists/* /tmp/runtime-packages.txt

# The libvips install tree. `include/` and the static/libtool archives are build
# inputs, not runtime ones, and are not copied or are removed here; `bin/`
# carries `vips` itself, which is the only thing in the image that LOADS these
# libraries and therefore the only available proof that they work at runtime.
COPY --from=vips /usr/local/lib/ /usr/local/lib/
COPY --from=vips /usr/local/bin/ /usr/local/bin/
RUN set -eux; \
    find /usr/local/lib -name '*.a' -delete; \
    find /usr/local/lib -name '*.la' -delete; \
    rm -rf /usr/local/lib/pkgconfig; \
    find /usr/local/lib -maxdepth 2 -type d -name pkgconfig -exec rm -rf {} +; \
    ldconfig; \
    vips --version; \
    vips -l > /dev/null

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
#
# /var/lib/vizra/media EXISTS HERE, owned by the runtime uid, before any code
# writes to it. The compose tree mounts a volume there, and Docker creates a
# mountpoint that is ABSENT from the image as root:root — so the first upload on
# a fresh host would fail with EACCES, on first boot, in front of the owner
# (meta PR #4, `vizra-infrastructure` seat, FINDING 5). Declaring it here costs
# nothing and is the only place it can be declared before the storage slice
# lands.
RUN set -eux; \
    groupadd --system --gid 10001 vizra; \
    useradd --system --uid 10001 --gid vizra --home /var/lib/vizra --create-home vizra; \
    mkdir -p /var/lib/vizra/media; \
    chown 10001:10001 /var/lib/vizra/media; \
    chmod 0750 /var/lib/vizra/media
USER 10001:10001

ENV VIZRA_LISTEN_ADDR=:8080 \
    VIZRA_METRICS_ADDR=127.0.0.1:9090
EXPOSE 8080

# A probe that CANNOT PASS while the service is broken: it requests the api's
# own /readyz over the loopback interface and exits non-zero on refused,
# timed-out or non-2xx (see internal/healthcheck). The previous
# `["CMD","/usr/local/bin/vizra","version"]` exited 0 whether or not anything
# was listening and whether or not PostgreSQL was reachable.
#
# This is the IMAGE default, which matches the image's default CMD (vizra-api).
# A worker container overrides both: `vizra healthcheck worker` reads the
# worker's readiness on its metrics listener. The compose tree sets that.
#
# --timeout=3s against the probe's own 2s deadline, so the probe always gets to
# report its reason instead of being killed by the runtime.
HEALTHCHECK --interval=15s --timeout=3s --start-period=20s --retries=3 \
    CMD ["/usr/local/bin/vizra", "healthcheck", "api"]

ENTRYPOINT ["/usr/bin/tini", "--"]
CMD ["/usr/local/bin/vizra-api"]
