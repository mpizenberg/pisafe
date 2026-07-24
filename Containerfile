FROM docker.io/library/node@sha256:af01d58b748ec92b1d6e8e11429aad424fd1e68c848185399dca0596a1ab8f5c

ARG PISAFE_RECIPE_DIGEST

RUN apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        ca-certificates \
        git \
        openssl \
        openssh-server \
        tini \
    && rm -rf /var/lib/apt/lists/* \
    && package="$(npm pack "@earendil-works/pi-coding-agent@0.82.0" --ignore-scripts --silent)" \
    && actual="$(openssl dgst -sha512 -binary "${package}" | openssl base64 -A)" \
    && test "sha512-${actual}" = "sha512-Qnqgn9zhJFQ2HZ8R4iNuGhyCk93XX6+eUw9i+TjTuo47amzCy93ft3bB6yaUCleCrNO58dJDHYSGNHv/GAPWKg==" \
    && npm install --global --ignore-scripts "./${package}" \
    && rm -f "${package}" \
    && npm cache clean --force \
    && mkdir -p /run/sshd \
    && rm -f /etc/ssh/ssh_host_* \
    && rm -rf /root/.npm /root/.cache

COPY --chmod=0755 pisafe-guest /usr/local/bin/pisafe-guest

LABEL io.pisafe.base.digest="sha256:af01d58b748ec92b1d6e8e11429aad424fd1e68c848185399dca0596a1ab8f5c" \
      io.pisafe.pi.version="0.82.0" \
      io.pisafe.recipe.digest="${PISAFE_RECIPE_DIGEST}" \
      org.opencontainers.image.title="pisafe run environment"

USER node
ENV HOME=/home/node \
    PI_CODING_AGENT_DIR=/home/node/.pi/agent \
    PI_SKIP_VERSION_CHECK=1
WORKDIR /work
ENTRYPOINT ["/usr/bin/tini", "--"]
CMD ["sleep", "infinity"]
