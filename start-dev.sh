#!/bin/sh
#
# Start golink locally, behind an nginx that authenticates nobody and tells
# golink that every visitor is one fixed person. See deploy/nginx/golink-dev.conf.
#
#   ./start-dev.sh          # http://localhost:8080/
#   ./start-dev.sh 80       # http://localhost/
#   ./start-dev.sh --mysql  # links in a MySQL container, not the SQLite file
#
# The two backends hold different links, so --mysql is a different set of them,
# not the same ones stored elsewhere. See compose.mysql.yaml.
#
# A port of 1024 or more is published on localhost only, since this stack has
# no authentication and a laptop on a shared network would otherwise be serving
# it to that network. A privileged port cannot be: podman will bind one only to
# every address, never to a single one, and it warns about that below. Either
# way no root is needed, because podman binds the port rather than golink.
#
# Data lives in ./data and survives this script, a rebuild, and podman compose
# down. With --mysql it lives in a named volume, which "podman compose down -v"
# throws away.
set -eu

compose_files="-f compose.yaml"
backend=SQLite
while [ $# -gt 0 ]; do
    case $1 in
        --mysql)  compose_files="-f compose.yaml -f compose.mysql.yaml"; backend=MySQL; shift ;;
        --sqlite) shift ;;
        --*)      echo "unknown option: $1" >&2; exit 2 ;;
        *)        break ;;
    esac
done

port=${1:-${GOLINK_PORT:-8080}}

if [ "$port" -lt 1024 ]; then
    publish="$port:80"
    exposed=yes
else
    publish="127.0.0.1:$port:80"
    exposed=no
fi
export GOLINK_PUBLISH="$publish"

# --remove-orphans so that switching between the two backends takes the
# container of the one being left with it.
podman compose $compose_files up -d --build --remove-orphans

# nginx resolves golink's address once, at startup, and a rebuild gives the
# container a new one. Without this, nginx serves 502 until it is restarted.
podman compose $compose_files restart nginx

# Poll for a 200 rather than leaving it to curl's --retry, which called the
# stack dead while it was merely still coming up: nginx had been asked to
# restart a moment earlier, and neither its refused connections nor its 502s
# were retried the way they were meant to be.
deadline=$(( $(date +%s) + 90 ))
while :; do
    code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "http://localhost:$port/" || true)
    [ "$code" = 200 ] && break
    [ "$(date +%s)" -lt "$deadline" ] || break
    sleep 1
done

if [ "$code" = 200 ]; then
    echo
    echo "golink is at http://localhost:$port/ -- every visitor is dev@wetravel.com,"
    echo "and the links are in $backend."
    echo "Try http://localhost:$port/fadsfads to see the create-on-404 form."
    echo "Audit log: podman compose $compose_files logs -f golink"
    if [ "$exposed" = yes ]; then
        echo
        echo "WARNING: port $port is published on every address, not just localhost,"
        echo "because podman cannot bind a privileged port to one address. Anyone who"
        echo "can reach this machine can create and delete links as dev@wetravel.com."
        echo "To keep it on localhost and still answer on port $port, run this on 8080"
        echo "and redirect instead:"
        echo
        echo "  echo \"rdr pass on lo0 inet proto tcp from any to 127.0.0.1 port $port -> 127.0.0.1 port 8080\" | sudo pfctl -Ef -"
    fi
else
    echo "golink did not come up (last response: ${code:-none}); try:" >&2
    echo "  podman compose $compose_files logs" >&2
    exit 1
fi
