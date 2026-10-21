#!/bin/sh
# Starts one Zotero client under Xvfb. Everything stateful lives in /work,
# which the harness mounts from the host so logs survive the container.
set -eu

: "${ZOTERO_PORT:?}" "${BRIDGE_PORT:?}"
E2E_UID="${E2E_UID:-0}"
E2E_GID="${E2E_GID:-0}"

# dbus refuses to start for a uid without a passwd entry, and the host user's
# uid is arbitrary. Create a matching account, then drop root so files in the
# mounted /work end up owned by the host user.
if [ "$(id -u)" = "0" ] && [ "$E2E_UID" != "0" ]; then
  getent group "$E2E_GID" >/dev/null || groupadd -g "$E2E_GID" e2e
  getent passwd "$E2E_UID" >/dev/null || useradd -o -u "$E2E_UID" -g "$E2E_GID" -M -d /work/home -s /bin/sh e2e
  exec setpriv --reuid="$E2E_UID" --regid="$E2E_GID" --clear-groups "$0" "$@"
fi

export HOME=/work/home
mkdir -p "$HOME"

# Zotero connector server only listens on loopback, which a published
# Docker port cannot reach, so a small HTTP bridge forwards requests to it.
node /opt/e2e/bridge.mjs "$BRIDGE_PORT" "$ZOTERO_PORT" &

# xvfb-run hung forever waiting for Xvfb's readiness signal in this setup
# (Xvfb was up, the wrapped command never started), so start the server by hand
# and wait for its socket instead. -ac is fine: the display is container-local.
export DISPLAY=:99
Xvfb "$DISPLAY" -screen 0 1280x1024x24 -nolisten tcp -ac >> /work/xvfb.log 2>&1 &
for _ in $(seq 1 50); do
  [ -S /tmp/.X11-unix/X99 ] && break
  sleep 0.2
done

# The launcher script and binary sit in the mounted tarball. -ZoteroDebugText
# prints the debug log on stdout, which is how we get a log file.
exec dbus-launch --exit-with-session \
  /zotero/zotero -profile /work/profile -no-remote -ZoteroDebugText \
  >> /work/zotero.log 2>&1
