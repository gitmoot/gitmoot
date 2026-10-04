# Rust review image: the omp-review base and runtime layout plus the toolchain
# jerryfane/herdr CI pins (rust-toolchain.toml, .github/workflows/ci.yml):
# Rust 1.96.1 with rustfmt and clippy, Zig 0.16.0, just, cargo-nextest, Bun,
# and a current git (herdr's release tests need `git diff --default-prefix`,
# git 2.41+; Debian 12 ships 2.39).
FROM e2bdev/base@sha256:4a369f01a820fe5e65f53c2c5727a78899daf86f0541b721097f289559c8b73f

USER root

ARG RUSTUP_VERSION=1.29.1
ARG RUSTUP_SHA256=dda7234360b7f578ca8b0ddcb80145646fa61a67c1720a5abc7051b35c9fcb71
ARG RUST_VERSION=1.96.1
ARG ZIG_VERSION=0.16.0
ARG ZIG_SHA256=70e49664a74374b48b51e6f3fdfbf437f6395d42509050588bd49abe52ba3d00
ARG JUST_VERSION=1.58.0
ARG JUST_SHA256=4a5cc2f53e6f0f8c59092a6cc38291eb729d46a7dd95d3ae582008881b84931d
ARG NEXTEST_VERSION=0.9.146
ARG NEXTEST_SHA256=682c21b777c333e96fd532e114d3a5a894e0729ab88d94c0a9f20f8419695428
ARG BUN_VERSION=1.3.14
ARG BUN_SHA256=951ee2aee855f08595aeec6225226a298d3fea83a3dcd6465c09cbccdf7e848f
ARG GIT_VERSION=2.56.0
ARG GIT_SHA256=26c56c296b38c0695b26fa95f475f1d01704d2d38e73465ca30b0b2f5dc789d3

# Built into /usr/local, ahead of the distribution's /usr/bin/git on PATH.
RUN set -eux; \
    apt-get update; \
    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      libcurl4-openssl-dev libexpat1-dev libssl-dev zlib1g-dev; \
    rm -rf /var/lib/apt/lists/*; \
    curl --fail --show-error --location --output /tmp/git.tar.xz \
      "https://mirrors.edge.kernel.org/pub/software/scm/git/git-${GIT_VERSION}.tar.xz"; \
    echo "${GIT_SHA256}  /tmp/git.tar.xz" | sha256sum --check --strict; \
    mkdir /tmp/git; \
    tar -xJf /tmp/git.tar.xz --directory /tmp/git --strip-components=1 --no-same-owner; \
    make -C /tmp/git -j"$(nproc)" prefix=/usr/local \
      NO_GETTEXT=1 NO_TCLTK=1 NO_PERL=1 NO_PYTHON=1 NO_RUST=1 all install; \
    rm -rf /tmp/git /tmp/git.tar.xz; \
    /usr/local/bin/git --version

# Dockerfile ENV does not reach sandbox processes: envd starts them with its own
# default PATH and HOME=/home/user. So rustup uses its default homes,
# ~/.rustup and ~/.cargo of UID 1000 (a repository's rust-toolchain.toml can
# then add a target or component at review time), and every tool is linked
# into /usr/local/bin, which is on envd's default PATH.
RUN set -eux; \
    curl --fail --show-error --location --output /tmp/rustup-init \
      "https://static.rust-lang.org/rustup/archive/${RUSTUP_VERSION}/x86_64-unknown-linux-gnu/rustup-init"; \
    echo "${RUSTUP_SHA256}  /tmp/rustup-init" | sha256sum --check --strict; \
    chmod 0755 /tmp/rustup-init

USER user

RUN set -eux; \
    HOME=/home/user RUSTUP_HOME=/home/user/.rustup CARGO_HOME=/home/user/.cargo \
      /tmp/rustup-init -y --no-modify-path --profile minimal \
      --default-toolchain "${RUST_VERSION}" --component rustfmt,clippy

USER root

RUN set -eux; \
    rm /tmp/rustup-init; \
    for tool in /home/user/.cargo/bin/*; do ln -s "$tool" "/usr/local/bin/$(basename "$tool")"; done

# E2B writes /etc/resolv.conf without a trailing newline, which Zig 0.16.0's
# resolver rejects (ResolvConfParseFailed), so `zig build` cannot fetch its
# package dependencies. Terminate the last line; the nameserver is unchanged.
# (`echo`, not `printf '\n'`: E2B's Dockerfile parser drops the backslash.)
RUN set -eux; \
    if [ -n "$(tail -c 1 /etc/resolv.conf)" ]; then echo >> /etc/resolv.conf; fi; \
    test -z "$(tail -c 1 /etc/resolv.conf)"; \
    cat /etc/resolv.conf

RUN set -eux; \
    curl --fail --show-error --location --output /tmp/zig.tar.xz \
      "https://ziglang.org/download/${ZIG_VERSION}/zig-x86_64-linux-${ZIG_VERSION}.tar.xz"; \
    echo "${ZIG_SHA256}  /tmp/zig.tar.xz" | sha256sum --check --strict; \
    mkdir /opt/zig; \
    tar -xJf /tmp/zig.tar.xz --directory /opt/zig --strip-components=1 --no-same-owner; \
    rm /tmp/zig.tar.xz; \
    ln -s /opt/zig/zig /usr/local/bin/zig

RUN set -eux; \
    curl --fail --show-error --location --output /tmp/just.tar.gz \
      "https://github.com/casey/just/releases/download/${JUST_VERSION}/just-${JUST_VERSION}-x86_64-unknown-linux-musl.tar.gz"; \
    echo "${JUST_SHA256}  /tmp/just.tar.gz" | sha256sum --check --strict; \
    tar -xzf /tmp/just.tar.gz --directory /usr/local/bin --no-same-owner just; \
    rm /tmp/just.tar.gz; \
    curl --fail --show-error --location --output /tmp/nextest.tar.gz \
      "https://github.com/nextest-rs/nextest/releases/download/cargo-nextest-${NEXTEST_VERSION}/cargo-nextest-${NEXTEST_VERSION}-x86_64-unknown-linux-gnu.tar.gz"; \
    echo "${NEXTEST_SHA256}  /tmp/nextest.tar.gz" | sha256sum --check --strict; \
    tar -xzf /tmp/nextest.tar.gz --directory /usr/local/bin --no-same-owner cargo-nextest; \
    rm /tmp/nextest.tar.gz; \
    curl --fail --show-error --location --output /tmp/bun.zip \
      "https://github.com/oven-sh/bun/releases/download/bun-v${BUN_VERSION}/bun-linux-x64.zip"; \
    echo "${BUN_SHA256}  /tmp/bun.zip" | sha256sum --check --strict; \
    unzip -j -d /usr/local/bin /tmp/bun.zip bun-linux-x64/bun; \
    rm /tmp/bun.zip; \
    chmod 0755 /usr/local/bin/just /usr/local/bin/cargo-nextest /usr/local/bin/bun

# Envd runs review commands as UID/GID 1000 (user). Same credential-free
# runtime directories as ../omp-review; Gitmoot uploads the OMP binary and the
# short-lived gateway identity after each sandbox is provisioned.
RUN /usr/bin/install -d -o 1000 -g 1000 -m 0700 \
      /home/user/.gitmoot \
      /home/user/.gitmoot/runtime \
      /home/user/.gitmoot/runtime/bin \
      /home/user/.gitmoot/runtime/attachments

RUN /usr/local/bin/zig version
RUN /usr/local/bin/just --version && /usr/local/bin/cargo-nextest nextest --version
RUN /usr/local/bin/bun --version
RUN /usr/bin/git --version
RUN /usr/bin/gcc --version
RUN /usr/local/bin/python3 --version

USER user

# As the runtime user: rustup proxies resolve the toolchain from ~/.rustup.
RUN export RUSTUP_AUTO_INSTALL=0 && rustc --version && cargo --version
RUN export RUSTUP_AUTO_INSTALL=0 && cargo fmt --version && cargo clippy --version
