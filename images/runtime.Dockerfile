# sbx-runtime: what runs next to every workspace (the sidecar), and what
# the workspace's own container runs from /sbx, mounted read-only from this
# image: the agent, and the editor (code-server and the activity extension).

FROM debian:trixie-slim AS editor
RUN apt-get update \
 && apt-get install -y --no-install-recommends curl ca-certificates \
 && rm -rf /var/lib/apt/lists/*
ARG TARGETARCH
ARG CODE_SERVER_VERSION=4.139.1
ARG CODE_SERVER_SHA256_amd64=53029be6c5781b7bca49b815fcc9a2a3fc111813ad8c9965b2c0f0d2985a0674
ARG CODE_SERVER_SHA256_arm64=0edb4b60d9c4744b2dd14b0911e3c2e6dd8c6f3c13bd58bda23ae744e59e7df1
RUN set -eu; \
    case "$TARGETARCH" in \
      amd64) sum="$CODE_SERVER_SHA256_amd64" ;; \
      arm64) sum="$CODE_SERVER_SHA256_arm64" ;; \
      *) echo "no code-server for $TARGETARCH" >&2; exit 1 ;; \
    esac; \
    curl -fsSL -o /tmp/code-server.tgz \
      "https://github.com/coder/code-server/releases/download/v${CODE_SERVER_VERSION}/code-server-${CODE_SERVER_VERSION}-linux-${TARGETARCH}.tar.gz"; \
    echo "$sum  /tmp/code-server.tgz" | sha256sum -c -; \
    mkdir -p /sbx/editor/code-server; \
    tar -xzf /tmp/code-server.tgz -C /sbx/editor/code-server --strip-components=1; \
    rm /tmp/code-server.tgz
# Built by `make extension`; the folder name is publisher.name-version.
COPY extensions/vscode-activity/package.json /sbx/editor/extensions/sbx.activity-0.1.0/
COPY extensions/vscode-activity/dist /sbx/editor/extensions/sbx.activity-0.1.0/dist

FROM debian:trixie-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends git ca-certificates \
 && rm -rf /var/lib/apt/lists/*
COPY --from=editor /sbx/editor /sbx/editor
ARG TARGETARCH
COPY bin/linux_${TARGETARCH}/sbx-sidecar bin/linux_${TARGETARCH}/sbx-agent /sbx/bin/
USER 1000:1000
ENTRYPOINT ["/sbx/bin/sbx-sidecar"]
