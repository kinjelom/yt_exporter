# ---------------------------------------------------------------------------
# yt_exporter - the exporter binary in a distroless image
#
# Two stages: build a static binary, then copy it into an image that holds
# nothing else - no shell, no package manager. Built by scripts/image.sh, which
# passes the version and the base images from release.conf:
#
#     docker run -p 9776:9776 -e YOUTRACK_URL=http://youtrack:8080 \
#       -e YOUTRACK_TOKEN=perm:... ghcr.io/kinjelom/yt_exporter
# ---------------------------------------------------------------------------

ARG GO_IMAGE=docker.io/library/golang:1.26-alpine
ARG RUNTIME_IMAGE=gcr.io/distroless/static-debian12:nonroot

FROM ${GO_IMAGE} AS build

ARG VERSION=dev
ARG REVISION=unknown
ARG BRANCH=unknown
ENV CGO_ENABLED=0

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -trimpath \
      -ldflags "-s -w \
        -X github.com/prometheus/common/version.Version=${VERSION} \
        -X github.com/prometheus/common/version.Revision=${REVISION} \
        -X github.com/prometheus/common/version.Branch=${BRANCH} \
        -X github.com/prometheus/common/version.BuildDate=$(date -u +%Y%m%d-%H:%M:%S)" \
      -o /out/yt_exporter ./cmd/yt_exporter
# the licenses of the code compiled into the binary, which travel with it (scripts/third-party-licenses.sh)
RUN sh scripts/third-party-licenses.sh --title "yt_exporter ${VERSION} for $(go env GOOS)/$(go env GOARCH)" \
      > /out/THIRD_PARTY_LICENSES.txt

FROM ${RUNTIME_IMAGE}

ARG VERSION=dev
ARG REVISION=unknown
ARG SOURCE_URL=https://github.com/kinjelom/yt_exporter

# org.opencontainers.image.source links the package on ghcr.io to the repository
LABEL org.opencontainers.image.title="yt_exporter" \
      org.opencontainers.image.description="Prometheus exporter for JetBrains YouTrack" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}" \
      org.opencontainers.image.source="${SOURCE_URL}" \
      org.opencontainers.image.licenses="MIT"

COPY --from=build /out/yt_exporter /bin/yt_exporter
COPY --from=build /out/THIRD_PARTY_LICENSES.txt /usr/share/doc/yt_exporter/THIRD_PARTY_LICENSES.txt
COPY LICENSE /usr/share/doc/yt_exporter/LICENSE
EXPOSE 9776
USER nonroot
ENTRYPOINT ["/bin/yt_exporter"]
