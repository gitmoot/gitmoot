# Node review image: the omp-review base and runtime layout plus the toolchain
# jerryfane/joltra CI uses (.github/workflows/ci.yml, package.json,
# pnpm-lock.yaml): Node 20, pnpm 10.13.1, ffmpeg, Playwright 1.60.0 Chromium
# (apps/web vitest suites launch it), and uv, which `pnpm test` uses to run the
# Python worker suite (uv fetches the repository's pinned CPython at run time).
FROM e2bdev/base@sha256:4a369f01a820fe5e65f53c2c5727a78899daf86f0541b721097f289559c8b73f

USER root

ARG NODE_VERSION=20.20.2
ARG NODE_SHA256=df770b2a6f130ed8627c9782c988fda9669fa23898329a61a871e32f965e007d
ARG PNPM_VERSION=10.13.1
ARG PLAYWRIGHT_VERSION=1.60.0
ARG UV_VERSION=0.12.23
ARG UV_SHA256=9167d72b3319674b6303c4cbe071854bba13ebdf3d76b1a7cbdc175471fb66d6

RUN set -eux; \
    apt-get update; \
    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends ffmpeg; \
    rm -rf /var/lib/apt/lists/*

# Replace the base image's Node 20.9.0 in /usr/local, which is on the default
# PATH of login and non-login shells alike.
RUN set -eux; \
    curl --fail --show-error --location --output /tmp/node.tar.xz \
      "https://nodejs.org/dist/v${NODE_VERSION}/node-v${NODE_VERSION}-linux-x64.tar.xz"; \
    echo "${NODE_SHA256}  /tmp/node.tar.xz" | sha256sum --check --strict; \
    rm -rf /usr/local/lib/node_modules/npm /usr/local/lib/node_modules/corepack \
      /usr/local/bin/node /usr/local/bin/npm /usr/local/bin/npx /usr/local/bin/corepack; \
    tar -xJf /tmp/node.tar.xz --directory /usr/local --strip-components=1 --no-same-owner; \
    rm /tmp/node.tar.xz; \
    npm install --global --no-fund --no-audit "pnpm@${PNPM_VERSION}"; \
    npm cache clean --force

# Chromium's system libraries need root; the browser itself goes to
# Playwright's default ~/.cache/ms-playwright of UID 1000, because Dockerfile
# ENV (such as PLAYWRIGHT_BROWSERS_PATH) does not reach sandbox processes.
# A repository that bumps Playwright can fetch its revision at review time.
RUN set -eux; \
    apt-get update; \
    npx --yes "playwright@${PLAYWRIGHT_VERSION}" install-deps chromium; \
    rm -rf /var/lib/apt/lists/* /root/.npm

USER user

RUN set -eux; \
    HOME=/home/user npx --yes "playwright@${PLAYWRIGHT_VERSION}" install chromium; \
    rm -rf /home/user/.npm; \
    ls /home/user/.cache/ms-playwright

USER root

RUN set -eux; \
    curl --fail --show-error --location --output /tmp/uv.tar.gz \
      "https://github.com/astral-sh/uv/releases/download/${UV_VERSION}/uv-x86_64-unknown-linux-gnu.tar.gz"; \
    echo "${UV_SHA256}  /tmp/uv.tar.gz" | sha256sum --check --strict; \
    tar -xzf /tmp/uv.tar.gz --directory /usr/local/bin --strip-components=1 --no-same-owner \
      uv-x86_64-unknown-linux-gnu/uv uv-x86_64-unknown-linux-gnu/uvx; \
    rm /tmp/uv.tar.gz

# Envd runs review commands as UID/GID 1000 (user). Same credential-free
# runtime directories as ../omp-review; Gitmoot uploads the OMP binary and the
# short-lived gateway identity after each sandbox is provisioned.
RUN /usr/bin/install -d -o 1000 -g 1000 -m 0700 \
      /home/user/.gitmoot \
      /home/user/.gitmoot/runtime \
      /home/user/.gitmoot/runtime/bin \
      /home/user/.gitmoot/runtime/attachments

RUN /usr/local/bin/node --version && /usr/local/bin/npm --version
RUN /usr/local/bin/pnpm --version
RUN /usr/local/bin/uv --version
RUN /usr/local/bin/python3 --version
RUN /usr/bin/ffmpeg -version
RUN /usr/bin/git --version

USER user
