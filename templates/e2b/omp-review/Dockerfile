FROM e2bdev/base@sha256:4a369f01a820fe5e65f53c2c5727a78899daf86f0541b721097f289559c8b73f

USER root

ARG GO_VERSION=1.26.4
ARG GO_SHA256=1153d3d50e0ac764b447adfe05c2bcf08e889d42a02e0fe0259bd47f6733ad7f

RUN set -eux; \
    curl --fail --show-error --location --output /tmp/go.tar.gz \
      "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz"; \
    echo "${GO_SHA256}  /tmp/go.tar.gz" | sha256sum --check --strict; \
    rm -rf /usr/local/go; \
    tar -C /usr/local -xzf /tmp/go.tar.gz; \
    rm /tmp/go.tar.gz

ENV PATH=/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
ENV GOTOOLCHAIN=local

# Envd runs review commands as UID/GID 1000 (user). Keep only reusable,
# credential-free directories in the image; Gitmoot uploads the OMP binary and
# short-lived gateway identity after each sandbox is provisioned.
RUN /usr/bin/install -d -o 1000 -g 1000 -m 0700 \
      /home/user/.gitmoot \
      /home/user/.gitmoot/runtime \
      /home/user/.gitmoot/runtime/bin \
      /home/user/.gitmoot/runtime/attachments

RUN /usr/local/go/bin/go version
RUN /usr/bin/git --version
RUN /usr/bin/curl --version
RUN /usr/bin/python3 --version
RUN /usr/bin/tar --version
RUN /usr/bin/unzip -v
RUN /usr/bin/make --version
RUN /usr/bin/gcc --version

USER user
