#!/bin/sh
#
# Import an export of links (JSON Lines, as /.export produces) into the local
# database at ./data/golink.db.
#
#   ./import.sh /private/tmp/links_export.json
#
# The restore adds links and skips any name that already exists, so it will not
# overwrite a link with a changed destination. Delete the link first, or edit it
# in the UI.
#
# On macOS use the real path: the podman VM has /private/tmp and /Users, and
# /tmp is only a symlink to the first of those, so mounting /tmp/... fails.
set -eu

file=${1:?usage: ./import.sh <export.jsonl>}
[ -f "$file" ] || { echo "no such file: $file" >&2; exit 1; }
case $file in
    /*) path=$file ;;
    *)  path=$(cd "$(dirname "$file")" && pwd)/$(basename "$file") ;;
esac

mkdir -p data

# Stop golink while another process writes its database.
running=$(podman compose ps --status running --services 2>/dev/null | grep -c '^golink$' || true)
[ "$running" = 0 ] || podman compose stop golink >/dev/null

# golink restores a snapshot at startup, and exits after resolving a link given
# as an argument, so this imports and stops rather than staying up to serve.
podman run --rm \
    -v ./data:/home/nonroot \
    -v "$path":/import.jsonl:ro \
    docker.io/library/tailscale-golink-golink \
    -sqlitedb=/home/nonroot/golink.db -snapshot=/import.jsonl -verbose . 2>&1 |
    grep -v '^$' || true

echo "links now in ./data/golink.db: $(sqlite3 data/golink.db 'SELECT count(*) FROM Links;' 2>/dev/null || echo '?')"

if [ "$running" != 0 ]; then
    podman compose start golink >/dev/null
    podman compose restart nginx >/dev/null
    echo "stack restarted"
fi
