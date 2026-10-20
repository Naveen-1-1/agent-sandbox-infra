# sbx-controller: the control plane (broker, and later the reconciler). Git is
# used to inspect source repositories.
FROM debian:trixie-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends git ca-certificates \
 && rm -rf /var/lib/apt/lists/*
ARG TARGETARCH
COPY bin/linux_${TARGETARCH}/sbx-controller /usr/local/bin/sbx-controller
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/sbx-controller"]
