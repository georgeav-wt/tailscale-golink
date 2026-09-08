# The nginx that fronts golink in the cluster, with its configuration baked in.
#
#   podman build -t golink_nginx -f deploy/nginx/Dockerfile deploy/nginx
#
# The configuration is in the image rather than in a ConfigMap so that one tag
# names one behaviour: a rollout carries the config with it, a rollback takes it
# back, and nothing has to reload nginx out of band. The cost is that a config
# change is a build and a deploy, which is the right price for a file that
# decides who golink believes you are.
#
# nginx-unprivileged, not nginx: it runs as uid 101 with its temporary paths and
# pid file under /tmp, so the pod needs no root and works with the read-only
# root filesystem the deployment chart sets by default.
FROM docker.io/nginxinc/nginx-unprivileged:1.29-alpine

# 00- and 01- sort ahead of the server block, so the map directives they hold
# are in place before anything reads the variables. default.conf takes over the
# name the image's own default server uses, which would otherwise answer for an
# unmatched Host.
COPY patterns.conf   /etc/nginx/conf.d/00-patterns.conf
COPY forwarded.conf  /etc/nginx/conf.d/01-forwarded.conf
COPY golink-k8s.conf /etc/nginx/conf.d/default.conf

EXPOSE 9292

# There is no "nginx -t" here on purpose: the server block names Services by
# their DNS names, which do not resolve during a build, so nginx -t fails on
# every image however good the config is. Check it by running the image with
# those names pointed anywhere -- which is what the build pipeline does:
#
#   podman run --rm -d --name golink-nginx-check -p 8081:9292 \
#       --read-only --tmpfs /tmp \
#       --add-host wt-golink:127.0.0.1 --add-host wt-oauth2-proxy:127.0.0.1 \
#       golink_nginx
#   curl -sf http://localhost:8081/healthcheck            # 200 ok
#   curl -so /dev/null -w '%{http_code} %{redirect_url}\n' \
#       http://localhost:8081/ABC-1234                    # 302 to Jira
