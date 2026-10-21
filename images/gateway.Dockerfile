# sbx-gateway: a static binary; needs nothing else.
FROM gcr.io/distroless/static-debian12:nonroot
ARG TARGETARCH
COPY bin/linux_${TARGETARCH}/sbx-gateway /sbx-gateway
ENTRYPOINT ["/sbx-gateway"]
