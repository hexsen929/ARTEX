# syntax=docker/dockerfile:1
# Runtime-only image. CI provides dist/<TARGETARCH>/artex; external intranet tools
# are deliberately not installed from unverified/floating download sources.
FROM python:3.12-slim-bookworm
# Official fixed-version Node archive, verified before installation. No mutable
# NodeSource setup script is piped into a shell. TARGETARCH is supplied by BuildKit.
ARG TARGETARCH
ARG NODE_VERSION=24.22.0
ARG NODE_DIST_BASE=https://nodejs.org/dist
ARG NPM_REGISTRY=https://registry.npmjs.org
ARG APT_MIRROR=
ARG PLAYWRIGHT_DOWNLOAD_HOST=
RUN if [ -n "$APT_MIRROR" ]; then \
      sed -i "s|deb.debian.org|${APT_MIRROR}|g; s|security.debian.org|${APT_MIRROR}|g" /etc/apt/sources.list.d/debian.sources; \
    fi \
    && apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates ripgrep curl wget vim git jq unzip \
      tzdata \
      dnsutils iputils-ping netcat-openbsd inetutils-telnet whois nmap \
    && rm -rf /var/lib/apt/lists/*
RUN set -eu; \
    case "$TARGETARCH" in amd64) node_arch=x64 ;; arm64) node_arch=arm64 ;; *) echo "Unsupported Node architecture: $TARGETARCH" >&2; exit 1 ;; esac; \
    archive="node-v${NODE_VERSION}-linux-${node_arch}.tar.gz"; \
    cd /tmp; \
    curl -fsSL -o "$archive" "${NODE_DIST_BASE}/v${NODE_VERSION}/$archive"; \
    curl -fsSL -o SHASUMS256.txt "${NODE_DIST_BASE}/v${NODE_VERSION}/SHASUMS256.txt"; \
    grep -F "  $archive" SHASUMS256.txt | sha256sum -c -; \
    tar -xzf "$archive" -C /usr/local --strip-components=1; \
    rm -f "$archive" SHASUMS256.txt
RUN npm config set registry "$NPM_REGISTRY" \
    && npm install -g @playwright/mcp@0.0.80 @playwright/cli@0.1.19 playwright@1.63.0 \
    && playwright-cli --help \
    && if [ -n "$PLAYWRIGHT_DOWNLOAD_HOST" ]; then \
         PLAYWRIGHT_DOWNLOAD_HOST="$PLAYWRIGHT_DOWNLOAD_HOST" playwright install --with-deps chromium; \
       else playwright install --with-deps chromium; fi \
    && rm -rf /var/lib/apt/lists/*
ENV TZ=Asia/Shanghai
COPY dist/${TARGETARCH}/artex /app/artex
WORKDIR /app
COPY start.sh /app/start.sh
COPY skills/ /app/skills/
COPY packaging/tools-manifest.json /app/packaging/tools-manifest.json
RUN mkdir -p /app/keys && chmod 700 /app/keys && chmod +x /app/artex /app/start.sh
ENV ARTEX_KEY_DIR=/app/keys
VOLUME ["/app/data", "/app/keys"]
EXPOSE 8787 8788
ENTRYPOINT ["/app/start.sh"]
CMD ["-addr", ":8787", "-proxy", ":8788"]
