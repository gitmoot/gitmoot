# Swift review image: the omp-review base and runtime layout plus the Linux
# Swift toolchain jerryfane/herdrup CI uses (`container: swift:6.1`).
FROM e2bdev/base@sha256:4a369f01a820fe5e65f53c2c5727a78899daf86f0541b721097f289559c8b73f

USER root

# Runtime dependencies of the swift.org Debian 12 toolchain, plus the login
# shells herdrup's LoginShellCommandTests exercise (its CI installs the same),
# and GNU awk for themartianapp/keephair's typecheck-harness gates (the base
# image's mawk rejects them).
RUN set -eux; \
    apt-get update; \
    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      binutils binutils-gold libc6-dev libcurl4-openssl-dev libedit2 \
      libgcc-12-dev libncurses-dev libpython3-dev libsqlite3-0 \
      libstdc++-12-dev libxml2-dev libz3-dev pkg-config tzdata uuid-dev \
      zlib1g-dev \
      fish zsh tcsh csh gawk; \
    rm -rf /var/lib/apt/lists/*

ARG SWIFT_VERSION=6.1.3
ARG SWIFT_SHA256=4db73efef24931f66045970d9da0d3d3b25980903c957941774932bdd1af1166

# The tarball's usr/ tree lands in /usr, so swift, swiftc and the SwiftPM tools
# are on the default PATH of login and non-login shells alike.
RUN set -eux; \
    curl --fail --show-error --location --output /tmp/swift.tar.gz \
      "https://download.swift.org/swift-${SWIFT_VERSION}-release/debian12/swift-${SWIFT_VERSION}-RELEASE/swift-${SWIFT_VERSION}-RELEASE-debian12.tar.gz"; \
    echo "${SWIFT_SHA256}  /tmp/swift.tar.gz" | sha256sum --check --strict; \
    tar -xzf /tmp/swift.tar.gz --directory / --strip-components=1 --no-same-owner; \
    rm /tmp/swift.tar.gz; \
    chmod -R o+r /usr/lib/swift

# Envd runs review commands as UID/GID 1000 (user). Same credential-free
# runtime directories as ../omp-review; Gitmoot uploads the OMP binary and the
# short-lived gateway identity after each sandbox is provisioned.
RUN /usr/bin/install -d -o 1000 -g 1000 -m 0700 \
      /home/user/.gitmoot \
      /home/user/.gitmoot/runtime \
      /home/user/.gitmoot/runtime/bin \
      /home/user/.gitmoot/runtime/attachments

RUN /usr/bin/swift --version
RUN /usr/bin/swift build --version
RUN /usr/bin/git --version
RUN /usr/bin/curl --version
RUN /usr/bin/fish --version && /usr/bin/zsh --version && test -x /usr/bin/tcsh && test -x /usr/bin/csh
RUN awk --version | grep -q 'GNU Awk'

USER user
