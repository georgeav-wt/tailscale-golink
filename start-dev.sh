#!/bin/sh
#
# Start golink locally, behind an nginx that authenticates nobody and tells
# golink that every visitor is one fixed person. See deploy/nginx/golink-dev.conf.
#
#   ./start-dev.sh          # http://localhost:8080/
#   ./start-dev.sh 80       # http://localhost/
#
# A port of 1024 or more is published on localhost only, since this stack has
# no authentication and a laptop on a shared network would otherwise be serving
# it to that network. A privileged port cannot be: podman will bind one only to
# every address, never to a single one, and it warns about that below. Either
# way no root is needed, because podman binds the port rather than golink.
#
# Data lives in the golink-data volume and survives this script, a rebuild, and
# podman compose down. "podman compose down -v" is what throws it away.
set -eu

port=${1:-${GOLINK_PORT:-8080}}

if [ "$port" -lt 1024 ]; then
    publish="$port:80"
    exposed=yes
else
    publish="127.0.0.1:$port:80"
    exposed=no
fi
export GOLINK_PUBLISH="$publish"

podman compose up -d --build --remove-orphans

# nginx resolves golink's address once, at startup, and a rebuild gives the
# container a new one. Without this, nginx serves 502 until it is restarted.
podman compose restart nginx

if curl -sf --retry 30 --retry-delay 1 --retry-connrefused --max-time 60 \
        -o /dev/null "http://localhost:$port/"; then
    echo
    echo "golink is at http://localhost:$port/ -- every visitor is dev@wetravel.com."
    echo "Try http://localhost:$port/fadsfads to see the create-on-404 form."
    echo "Audit log: podman compose logs -f golink"
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
    echo "golink did not come up; try: podman compose logs" >&2
    exit 1
fi
