# syntax=docker/dockerfile:1

FROM debian:trixie-slim

ARG TARGETARCH
ARG PACKAGE
ARG BINARY
ARG VERSION
ARG REVISION
ARG CREATED
ARG IMAGE_TITLE
ARG INSTALL_WORKBENCH=false

LABEL org.opencontainers.image.title="${IMAGE_TITLE}" \
      org.opencontainers.image.description="Harness command-line binary" \
      org.opencontainers.image.source="https://github.com/ClarifiedLabs/harness" \
      org.opencontainers.image.url="https://github.com/ClarifiedLabs/harness" \
      org.opencontainers.image.documentation="https://github.com/ClarifiedLabs/harness#readme" \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}" \
      org.opencontainers.image.created="${CREATED}"

COPY dist/${PACKAGE}_${VERSION}_${TARGETARCH}.deb /tmp/package.deb

RUN set -eux; \
    export DEBIAN_FRONTEND=noninteractive; \
    packages="ca-certificates"; \
    if [ "${INSTALL_WORKBENCH}" = "true" ]; then \
        packages="${packages} git openssh-client ripgrep"; \
    fi; \
    apt-get update; \
    apt-get install -y --no-install-recommends ${packages} /tmp/package.deb; \
    rm -rf /var/lib/apt/lists/* /tmp/package.deb; \
    ln -sf "/usr/bin/${BINARY}" /usr/local/bin/harness-entrypoint

ENTRYPOINT ["/usr/local/bin/harness-entrypoint"]
