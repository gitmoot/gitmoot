# Flutter review image: the omp-review base and runtime layout plus the Flutter
# SDK themartianapp/among-friends and jerryfane/numbra pin in .mise.toml and
# install in CI (subosito/flutter-action with that version).
FROM e2bdev/base@sha256:4a369f01a820fe5e65f53c2c5727a78899daf86f0541b721097f289559c8b73f

USER root

ARG FLUTTER_VERSION=3.41.9
ARG FLUTTER_SHA256=cf2631dde02570733921a530f47a96abe896b5e334682d2743c29530ea88bb2e

RUN set -eux; \
    apt-get update; \
    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends xz-utils zip; \
    rm -rf /var/lib/apt/lists/*

# The SDK writes into its own tree (bin/cache, version stamps) whenever it
# runs, so /opt/flutter belongs to UID 1000. The Dart SDK and the universal
# test artifacts (flutter_tester) are fetched here, not at review time.
# flutter and dart are linked into /usr/local/bin, which is on the default PATH
# of login and non-login shells alike.
RUN set -eux; \
    curl --fail --show-error --location --output /tmp/flutter.tar.xz \
      "https://storage.googleapis.com/flutter_infra_release/releases/stable/linux/flutter_linux_${FLUTTER_VERSION}-stable.tar.xz"; \
    echo "${FLUTTER_SHA256}  /tmp/flutter.tar.xz" | sha256sum --check --strict; \
    tar -xJf /tmp/flutter.tar.xz --directory /opt --no-same-owner; \
    rm /tmp/flutter.tar.xz; \
    ln -s /opt/flutter/bin/flutter /usr/local/bin/flutter; \
    ln -s /opt/flutter/bin/dart /usr/local/bin/dart; \
    git config --system --add safe.directory /opt/flutter; \
    flutter --disable-analytics; \
    flutter precache --universal --no-android --no-ios --no-web --no-linux; \
    chown -R 1000:1000 /opt/flutter

# Envd runs review commands as UID/GID 1000 (user). Same credential-free
# runtime directories as ../omp-review; Gitmoot uploads the OMP binary and the
# short-lived gateway identity after each sandbox is provisioned.
RUN /usr/bin/install -d -o 1000 -g 1000 -m 0700 \
      /home/user/.gitmoot \
      /home/user/.gitmoot/runtime \
      /home/user/.gitmoot/runtime/bin \
      /home/user/.gitmoot/runtime/attachments

RUN /usr/bin/git --version
RUN /usr/bin/curl --version
RUN /usr/bin/unzip -v

USER user

RUN flutter --disable-analytics && flutter --version && dart --version
