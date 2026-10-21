# Generic runner for real Zotero desktop builds. Zotero itself is deliberately
# NOT baked in: run.sh/fetch-zotero.mjs unpack the tarball into .cache/ on the
# host and the container mounts it read-only at /zotero. That keeps one image
# for every version (no 100 MB layer per release), lets CI cache the tarballs
# with actions/cache, and means we never redistribute Zotero binaries.
FROM node:22-bookworm-slim AS node

FROM debian:bookworm-slim
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates curl procps \
      xvfb xauth dbus-x11 \
      libgtk-3-0 libdbus-glib-1-2 libasound2 libx11-xcb1 libxt6 libxtst6 \
      libxcomposite1 libxdamage1 libxrandr2 libxfixes3 libxcursor1 libxi6 \
      libxss1 libpci3 libegl1 libgl1 \
      fonts-dejavu-core fonts-liberation \
    && rm -rf /var/lib/apt/lists/* \
    && mkdir -p /tmp/.X11-unix && chmod 1777 /tmp/.X11-unix

# Node only runs the tiny port bridge below; the test driver itself lives on
# the host (or CI runner) and talks to the bridge over a published port.
COPY --from=node /usr/local/bin/node /usr/local/bin/node
COPY container/ /opt/e2e/
RUN chmod +x /opt/e2e/entrypoint.sh

ENTRYPOINT ["/opt/e2e/entrypoint.sh"]
