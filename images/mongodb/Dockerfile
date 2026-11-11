# check=skip=InvalidDefaultArgInFrom
ARG GO_IMAGE
ARG BASE_IMAGE

FROM --platform=$BUILDPLATFORM ${GO_IMAGE} AS wrapper
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src/engine-mongodb
COPY --from=modules . /src/
COPY go.mod go.sum* ./
COPY cmd ./cmd
COPY engine ./engine
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags "-s -w" -o /out/databasify ./cmd/databasify

FROM --platform=$BUILDPLATFORM ${GO_IMAGE} AS pbm
ARG TARGETOS
ARG TARGETARCH
ARG PBM_VERSION
ARG PBM_SHA256
WORKDIR /src
RUN set -eux; \
    curl -fsSL -o /tmp/pbm.tar.gz "https://github.com/percona/percona-backup-mongodb/archive/refs/tags/v${PBM_VERSION}.tar.gz"; \
    echo "${PBM_SHA256}  /tmp/pbm.tar.gz" | sha256sum -c -; \
    tar -xzf /tmp/pbm.tar.gz --strip-components=1; \
    for tool in pbm pbm-agent; do \
        CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -mod=vendor -trimpath \
            -ldflags "-s -w -X github.com/percona/percona-backup-mongodb/pbm/version.version=${PBM_VERSION}" \
            -o "/out/$tool" "./cmd/$tool"; \
    done

FROM --platform=$BUILDPLATFORM ${GO_IMAGE} AS pcsm
ARG TARGETOS
ARG TARGETARCH
ARG PCSM_VERSION
ARG PCSM_SHA256
WORKDIR /src
RUN set -eux; \
    curl -fsSL -o /tmp/pcsm.tar.gz "https://github.com/percona/percona-clustersync-mongodb/archive/refs/tags/v${PCSM_VERSION}.tar.gz"; \
    echo "${PCSM_SHA256}  /tmp/pcsm.tar.gz" | sha256sum -c -; \
    tar -xzf /tmp/pcsm.tar.gz --strip-components=1; \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -buildvcs=false -tags=performance \
        -ldflags "-s -w -X main.Platform=$TARGETOS -X main.GitBranch=v${PCSM_VERSION}" \
        -o /out/pcsm .

FROM ${BASE_IMAGE}
ARG RELEASE
ARG SDK_VERSION
ARG LINE
ARG PACKAGES
RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends $PACKAGES; \
    rm -rf /var/lib/apt/lists/* /usr/local/bin/gosu
COPY --from=pbm /out/pbm /out/pbm-agent /usr/local/bin/
COPY --from=pcsm /out/pcsm /usr/local/bin/pcsm
RUN set -eux; \
    groupmod -g 64999 mongodb; \
    usermod -u 64999 -g 64999 mongodb; \
    find / -xdev \( -uid 999 -o -gid 999 \) -exec chown -h 64999:64999 {} +
COPY --from=wrapper /out/databasify /usr/local/bin/databasify
LABEL org.opencontainers.image.source="https://github.com/databasifylabs/engine-mongodb" \
      io.databasify.engine="mongodb" \
      io.databasify.release="${RELEASE}" \
      io.databasify.line="${LINE}" \
      io.databasify.sdk="${SDK_VERSION}"
ENV DATABASIFY_ENGINE=mongodb \
    DATABASIFY_RELEASE=${RELEASE} \
    DATABASIFY_LINE=${LINE} \
    GLIBC_TUNABLES=glibc.pthread.rseq=1
STOPSIGNAL SIGTERM
ENTRYPOINT ["/usr/local/bin/databasify", "serve"]
CMD []
