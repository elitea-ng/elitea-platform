# Debian 13 (trixie), the release every image in this repository is based on.
# It was alpine:3.20. coreutils, findutils, mawk and bash are part of the base,
# so `du -sb` (metrics.sh) and md5sum work as before. awscli is the Debian
# trixie package (AWS CLI v2).
FROM debian:trixie-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates \
        curl \
        wget \
        jq \
        coreutils \
        bash \
        awscli \
    && rm -rf /var/lib/apt/lists/*

COPY entrypoint.sh /usr/local/bin/entrypoint.sh
COPY metrics.sh /usr/local/bin/metrics.sh
RUN chmod +x /usr/local/bin/entrypoint.sh /usr/local/bin/metrics.sh

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
