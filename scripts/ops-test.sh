#!/usr/bin/env bash
# Operations test against real images, in three parts:
#
# 1. Upgrade and crashes: simulated Zotero clients sync with the image built
#    from OLD_REF (default: HEAD, the last commit). The first restart kills
#    it and starts the image built from the working tree on the same
#    database and files; later restarts kill that one again. The clients
#    must converge without losing anything.
# 2. The conformance kit against the upgraded server and its data.
# 3. Backup and restore as docs/install.md describes: clients build up
#    libraries, the database and files are backed up, everything is
#    destroyed and restored into new containers, and every client must sync
#    with the restored server cleanly and find it identical.
#
#   scripts/ops-test.sh [OLD_REF]
#
# Uses its own containers, network and volumes, all named gotero-ops-*, and
# removes them afterwards. SYNCSIM_RUNS and GOTERO_SYNCSIM_RESTART_RATE pass
# through.
set -euo pipefail

old_ref="${1:-HEAD}"
root="$(cd "$(dirname "$0")/.." && pwd)"
port="${GOTERO_OPS_PORT:-18080}"
name=gotero-ops
admin_pass=ops-test-admin-password
work="$(mktemp -d)"

cleanup() {
  docker rm -f "$name-app" "$name-pg" >/dev/null 2>&1 || true
  docker network rm "$name-net" >/dev/null 2>&1 || true
  docker volume rm "$name-pgdata" "$name-files" >/dev/null 2>&1 || true
  git -C "$root" worktree remove --force "$work/old" >/dev/null 2>&1 || true
  rm -rf "$work"
}
trap cleanup EXIT
cleanup

echo "==> building $old_ref and the working tree"
git -C "$root" worktree add --detach "$work/old" "$old_ref" >/dev/null
docker build -q -f "$work/old/deploy/Dockerfile" -t "$name:old" "$work/old" >/dev/null
docker build -q -f "$root/deploy/Dockerfile" -t "$name:new" "$root" >/dev/null
docker network create "$name-net" >/dev/null

# Each step is a small script, because the Go tests run some of them as
# their restart and restore hooks.
cat >"$work/start-pg.sh" <<EOF
#!/bin/sh
docker run -d --name $name-pg --network $name-net -v $name-pgdata:/var/lib/postgresql \\
  -e POSTGRES_USER=gotero -e POSTGRES_PASSWORD=gotero -e POSTGRES_DB=gotero postgres:18-alpine >/dev/null
# The image restarts once after initialising, so wait for a real query.
until docker exec $name-pg psql -U gotero -d gotero -c 'select 1' >/dev/null 2>&1; do sleep 1; done
EOF
cat >"$work/start.sh" <<EOF
#!/bin/sh
docker run -d --name $name-app --network $name-net -p 127.0.0.1:$port:8080 -v $name-files:/data \\
  -e GOTERO_DATABASE_URL=postgres://gotero:gotero@$name-pg:5432/gotero?sslmode=disable \\
  -e GOTERO_SECRET=ops-test-secret-0123456789abcdef0123456789 \\
  -e GOTERO_PUBLIC_URL=http://localhost:$port \\
  -e GOTERO_ADMIN_USERNAME=admin -e GOTERO_ADMIN_PASSWORD=$admin_pass \\
  -e GOTERO_LOG_FORMAT=text "\$1" >/dev/null
EOF
# docker kill, not stop: no graceful shutdown, like a crash or an OOM kill.
cat >"$work/restart.sh" <<EOF
#!/bin/sh
docker kill $name-app >/dev/null 2>&1
docker rm -f $name-app >/dev/null 2>&1
exec sh $work/start.sh $name:new
EOF
# The backup is taken while the server runs, as an operator would.
cat >"$work/backup-restore.sh" <<EOF
#!/bin/sh
set -e
docker exec $name-pg pg_dump -U gotero gotero >$work/gotero.sql
docker run --rm -v $name-files:/data -v $work:/backup alpine tar czf /backup/files.tgz -C /data .
docker rm -f $name-app $name-pg >/dev/null
docker volume rm $name-pgdata $name-files >/dev/null
sh $work/start-pg.sh
docker exec -i $name-pg psql -q -v ON_ERROR_STOP=1 -U gotero gotero <$work/gotero.sql >/dev/null
docker run --rm -v $name-files:/data -v $work:/backup alpine \\
  sh -c 'tar xzf /backup/files.tgz -C /data && chown -R 65532:65532 /data'
exec sh $work/start.sh $name:new
EOF

export GOTERO_CONFORMANCE_URL="http://localhost:$port/api/"
export GOTERO_CONFORMANCE_STREAM_URL="ws://localhost:$port/stream"
export GOTERO_CONFORMANCE_ADMIN_USER=admin
export GOTERO_CONFORMANCE_ADMIN_PASS="$admin_pass"
healthy() { until curl -fsS "http://localhost:$port/healthz" >/dev/null 2>&1; do sleep 1; done; }
in_conformance() { (cd "$root/conformance" && "$@"); }

echo "==> starting $old_ref"
sh "$work/start-pg.sh"
sh "$work/start.sh" "$name:old"
healthy

status=0
echo "==> syncing through the upgrade and repeated crashes"
GOTERO_SYNCSIM_RESTART_CMD="sh $work/restart.sh" \
  in_conformance go test -count=1 -v -timeout 60m ./gotero -run 'TestSyncSimRestart$' || status=$?
image="$(docker inspect -f '{{.Config.Image}}' "$name-app" 2>/dev/null || true)"
if [ "$status" -eq 0 ] && [ "$image" != "$name:new" ]; then
  echo "the simulation never restarted the server, so nothing was upgraded" >&2
  status=1
fi
if [ "$status" -eq 0 ]; then
  echo "==> conformance kit against the upgraded server"
  in_conformance go test -count=1 -timeout 30m ./gotero -run 'TestConformance$' || status=$?
fi
if [ "$status" -eq 0 ]; then
  echo "==> backup, total loss, restore"
  GOTERO_SYNCSIM_BACKUP_RESTORE_CMD="sh $work/backup-restore.sh" \
    in_conformance go test -count=1 -v -timeout 30m ./gotero -run 'TestSyncSimBackupRestore$' || status=$?
fi
if [ "$status" -ne 0 ]; then
  echo "==> server log" >&2
  docker logs --tail 200 "$name-app" >&2 || true
fi
exit "$status"
