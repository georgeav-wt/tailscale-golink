#!/bin/sh
#
# Import an export of links (JSON Lines, as /.export produces) into the local
# database at ./data/golink.db, or into the MySQL container with --mysql.
#
#   ./import.sh /private/tmp/links_export.json
#   ./import.sh --mysql /private/tmp/links_export.json
#
# The restore adds links and skips any name that already exists, so it will not
# overwrite a link with a changed destination. Delete the link first, or edit it
# in the UI.
#
# On macOS use the real path: the podman VM has /private/tmp and /Users, and
# /tmp is only a symlink to the first of those, so mounting /tmp/... fails.
set -eu

compose_files="-f compose.yaml"
mysql=no
while [ $# -gt 0 ]; do
    case $1 in
        --mysql)  compose_files="-f compose.yaml -f compose.mysql.yaml"; mysql=yes; shift ;;
        --sqlite) shift ;;
        --*)      echo "unknown option: $1" >&2; exit 2 ;;
        *)        break ;;
    esac
done

file=${1:?usage: ./import.sh [--mysql] <export.jsonl>}
[ -f "$file" ] || { echo "no such file: $file" >&2; exit 1; }
case $file in
    /*) path=$file ;;
    *)  path=$(cd "$(dirname "$file")" && pwd)/$(basename "$file") ;;
esac

mkdir -p data

running=$(podman compose $compose_files ps --status running --services 2>/dev/null | grep -c '^golink$' || true)
if [ "$mysql" = no ] && [ "$running" != 0 ]; then
    # Two processes must not write one SQLite file. MySQL is the whole point of
    # the other backend, so there it stays up.
    podman compose $compose_files stop golink >/dev/null
    stopped=yes
else
    stopped=no
fi

# golink restores a snapshot at startup, and exits after resolving a link given
# as an argument, so this imports and stops rather than staying up to serve. It
# runs as a one-off container of the golink service, which is what puts it on
# the same network and gives it the same database as the stack.
if [ "$mysql" = yes ]; then
    # The DSN comes from the service's environment, so no database is named here.
    db_args=
else
    db_args=-sqlitedb=/home/nonroot/golink.db
fi
# --build so that the import runs the current code. It means the first import
# after a change to the source recompiles golink in the image, which takes a
# minute or two; the alternative is an import that silently runs last week's
# code, which is how "--sqlitedb is required" came out of a run whose
# environment plainly had a DSN in it.
#
# golink is given "." as the link to resolve, so it stops after the import
# rather than staying up to serve. There is no such link, so it exits non-zero
# and compose says so: that line is the expected end of an import, not a
# failure, so it is dropped rather than left to look like one.
#
# shellcheck disable=SC2086 # db_args is empty on purpose for MySQL
podman compose $compose_files run --rm --build \
    -v "$path":/import.jsonl:ro \
    golink $db_args -snapshot=/import.jsonl -verbose . 2>&1 |
    grep -vE '^$|^Error: executing .*-snapshot=/import\.jsonl' || true

if [ "$mysql" = yes ]; then
    count=$(podman compose $compose_files exec -T mysql \
        mysql -ugolink -pgolink golink -N -B -e 'SELECT count(*) FROM Links;' 2>/dev/null || echo '?')
    echo "links now in the MySQL container: $count"
else
    echo "links now in ./data/golink.db: $(sqlite3 data/golink.db 'SELECT count(*) FROM Links;' 2>/dev/null || echo '?')"
fi

if [ "$stopped" = yes ]; then
    podman compose $compose_files start golink >/dev/null
    podman compose $compose_files restart nginx >/dev/null
    echo "stack restarted"
fi
