# Demo "customer office server": the released tunr CLI plus psql — installed
# the way a customer would, from the GitHub release (checksum-verified).
# Build context: this directory.
FROM alpine:3.20
ARG TUNR_VERSION=0.8.0
ARG TARGETARCH
RUN apk add --no-cache bash curl postgresql16-client ca-certificates tzdata \
 && cd /tmp \
 && base="https://github.com/tunr-dev/tunr/releases/download/v${TUNR_VERSION}" \
 && f="tunr_${TUNR_VERSION}_linux_${TARGETARCH:-amd64}.tar.gz" \
 && curl -fsSLO "$base/$f" && curl -fsSLO "$base/checksums.txt" \
 && grep " $f\$" checksums.txt | sha256sum -c - \
 && tar -xzf "$f" tunr && install -m 0755 tunr /usr/local/bin/tunr \
 && rm -f /tmp/* && tunr --version
COPY connector.sh /demo/connector.sh
ENTRYPOINT []
