# sbx-fixtures (tests and demo only): serves the repositories under
# test/fixtures/repos as read-only git over plain HTTP, so e2e tests and
# `make demo` don't depend on the internet.
FROM debian:trixie-slim AS build
RUN apt-get update && apt-get install -y --no-install-recommends git && rm -rf /var/lib/apt/lists/*
COPY test/fixtures/repos /repos
ENV GIT_AUTHOR_NAME=fixtures GIT_AUTHOR_EMAIL=fixtures@sbx.local GIT_AUTHOR_DATE="2026-01-01T00:00:00Z" \
    GIT_COMMITTER_NAME=fixtures GIT_COMMITTER_EMAIL=fixtures@sbx.local GIT_COMMITTER_DATE="2026-01-01T00:00:00Z"
RUN set -e; mkdir -p /srv; for d in /repos/*/; do \
      name=$(basename "$d"); \
      git -C "$d" init -q -b main; git -C "$d" add -A; git -C "$d" commit -q -m "initial"; \
      git clone -q --bare "$d" "/srv/$name.git"; git -C "/srv/$name.git" update-server-info; \
    done

FROM busybox:1.37
COPY --from=build /srv /srv
USER 65534:65534
EXPOSE 8080
ENTRYPOINT ["httpd", "-f", "-v", "-p", "8080", "-h", "/srv"]
