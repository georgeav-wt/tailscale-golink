#!/bin/sh
#
# Start the production arrangement -- nginx authenticating every request
# through a real oauth2-proxy against Google -- on this machine. This is for
# checking that arrangement before deploying it, not for deploying it: the
# cluster runs the same three parts from its own manifests.
#
#   GOLINK_OAUTH_CLIENT_ID=... GOLINK_OAUTH_CLIENT_SECRET=... ./start-prod.sh
#
# Add --mysql to keep the links in a MySQL container instead of the SQLite file,
# which is the arrangement the cluster uses. The two hold different links; see
# compose.mysql.yaml.
#
# The Google OAuth client needs http://localhost:8080/oauth2/callback among its
# authorised redirect URIs. Google allows http only for localhost, so a
# different host here needs https and a matching GOLINK_REDIRECT_URL.
#
# It shares the database with start-dev.sh, so the links are the same ones; the
# difference is who nginx says you are.
#
# Two secrets matter once more than one instance serves the same links, and both
# are generated here when unset, which is right for one machine and wrong for a
# Deployment: GOLINK_COOKIE_SECRET, which oauth2-proxy encrypts its session
# with, and GOLINK_XSRF_KEY, which golink signs its forms with. Give both from a
# Secret in the cluster, the same value to every pod.
set -eu

compose_files="-f compose.prod.yaml"
backend=SQLite
while [ $# -gt 0 ]; do
    case $1 in
        --mysql)  compose_files="-f compose.prod.yaml -f compose.mysql.yaml"; backend=MySQL; shift ;;
        --sqlite) shift ;;
        --*)      echo "unknown option: $1" >&2; exit 2 ;;
        *)        break ;;
    esac
done

missing=
for var in GOLINK_OAUTH_CLIENT_ID GOLINK_OAUTH_CLIENT_SECRET; do
    eval "value=\${$var:-}"
    [ -n "$value" ] || missing="$missing $var"
done
if [ -n "$missing" ]; then
    echo "set these first:$missing" >&2
    echo "they are the credentials of a Google OAuth 2.0 client of type Web application." >&2
    exit 1
fi

# oauth2-proxy encrypts its session cookie with this. Any 32 bytes will do for
# a local run; the deployment keeps a fixed one in a Secret, so that restarting
# does not sign everybody out.
if [ -z "${GOLINK_COOKIE_SECRET:-}" ]; then
    GOLINK_COOKIE_SECRET=$(openssl rand -base64 32 | tr -- '+/' '-_')
    export GOLINK_COOKIE_SECRET
    echo "generated a cookie secret for this run; set GOLINK_COOKIE_SECRET to keep sessions across restarts"
fi

# golink signs the XSRF tokens in its forms with this. One instance can invent
# its own; two cannot, because a form rendered by one would be refused by the
# other, so the deployment keeps a fixed one in a Secret.
if [ -z "${GOLINK_XSRF_KEY:-}" ]; then
    GOLINK_XSRF_KEY=$(openssl rand -base64 32 | tr -- '+/' '-_')
    export GOLINK_XSRF_KEY
    echo "generated an XSRF key for this run; set GOLINK_XSRF_KEY to share one between instances"
fi

port=${1:-${GOLINK_PORT:-8080}}
if [ "$port" -lt 1024 ]; then
    export GOLINK_PUBLISH="$port:80"
else
    export GOLINK_PUBLISH="127.0.0.1:$port:80"
fi
export GOLINK_REDIRECT_URL="${GOLINK_REDIRECT_URL:-http://localhost:$port/oauth2/callback}"

podman compose $compose_files up -d --build --remove-orphans
podman compose $compose_files restart nginx

echo
echo "golink is at http://localhost:$port/ behind Google sign-in, links in $backend."
echo "A request with no session is sent to Google; the identity comes back on"
echo "the headers golink is told to read. If sign-in loops, check that the"
echo "redirect URI registered with Google matches GOLINK_REDIRECT_URL."
