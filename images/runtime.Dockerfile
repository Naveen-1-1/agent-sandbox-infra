# sbx-runtime: the sidecar and agent that run in every workspace pod, plus git.
FROM debian:trixie-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends git ca-certificates \
 && rm -rf /var/lib/apt/lists/*
ARG TARGETARCH
COPY bin/linux_${TARGETARCH}/sbx-sidecar bin/linux_${TARGETARCH}/sbx-agent /sbx/bin/
USER 1000:1000
ENTRYPOINT ["/sbx/bin/sbx-sidecar"]
