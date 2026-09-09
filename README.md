# golink

`go/thing` shortlinks for WeTravel: memorable names for the pages people
actually use, a create form on every 404, and links anyone can fix.

This is a **fork of [tailscale/golink](https://github.com/tailscale/golink)**
(BSD-3-Clause), **heavily modified for internal use**. Upstream is built around a
Tailscale tailnet: it joins one, takes each visitor's identity from it, and reads
admin rights out of a tailnet policy file. None of that is available here, so
this fork runs as a plain HTTP service behind nginx and oauth2-proxy, takes
identity from headers that proxy sets, keeps admins in a database table, and can
store links in MySQL so more than one instance can serve them. The permission
model is inverted too: links are open for anyone to edit unless somebody locks
one.

The tailnet code is still compiled in and untouched, so a rebase on upstream
stays cheap, but nothing here uses it. See [Running on a tailnet](#running-on-a-tailnet).

![The go/ home page: the create form with its dynamic-link checkbox, and the most
visited links](screenshot.png)

`CLAUDE.md` in this repo is the long version: every change, why it went that way,
and what was tried and rejected. This file is how to use and deploy the thing.

## Contents

- [What is different from upstream](#what-is-different-from-upstream)
- [Creating links](#creating-links) — names, dynamic links, special URLs
- [Permissions](#permissions) — open by default, locking, the Admins table
- [Creating links from a script](#creating-links-from-a-script)
- [Running it locally](#running-it-locally) — SQLite and MySQL
- [Google SSO](#google-sso) — including testing on localhost without TLS
- [Configuration](#configuration) — flags and the config file
- [Running more than one instance](#running-more-than-one-instance)
- [Health checks](#health-checks)
- [Audit log and webhook](#audit-log-and-webhook)
- [Backups](#backups)
- [Data model](#data-model)
- [Deploying to Kubernetes](#deploying-to-kubernetes)
- [Browser configuration](#browser-configuration)
- [Running on a tailnet](#running-on-a-tailnet)
- [Licence](#licence)

## What is different from upstream

| | upstream | here |
|---|---|---|
| Identity | tailnet (`WhoIs`) | a header set by an authenticating proxy (`-auth-email-header`) |
| Admins | tailnet policy grants | rows in an `Admins` table, users or `group:` rows |
| Permissions | a link belongs to whoever made it | open to everyone with `-open-links`; an admin can lock one |
| Names | one segment | slashes allowed, so `go/team` and `go/team/jira` are separate links |
| Destinations | one field, both URL and template | two fields: a destination, and a *pattern* for the paths below the name |
| Storage | SQLite | SQLite or MySQL (`-mysql`), which lets several instances share the links |
| Instances | one | several, given a shared `-xsrf-key` |
| Search | by owner | by name, destination or pattern, from the link list |
| Audit | none | a JSON line per change on stdout, and optionally a webhook |
| Options | flags | flags or a config file (`-config`) |
| Health | none | `-healthcheck-path`, which reports the database |

## Creating links

Every link has a **name** and a **destination**. Visiting a name that does not
exist yet shows the create form with the name filled in, which is the whole point
of the tool: `go/onboarding` is a 404 and a form, not a dead end.

### Names

- letters, numbers, hyphens and periods
- **slashes are allowed**, and each segment must start with a letter or number,
  so `go/team/jira` is a name in its own right — there is no inheritance from
  `go/team`, and each has its own owner, lock and clicks
- not case-sensitive: `go/Foo` is `go/foo`
- hyphens are ignored when resolving: `go/meetingnotes` finds `go/meeting-notes`
- a name may not start with a dot; those are golink's own URLs

A destination is used **exactly as written**. If it has no scheme, `https://` is
added when the link is saved and the success page says so — `g.co/test` would
otherwise be resolved by the browser against golink itself.

### Dynamic links

A link with only a destination answers to its own name and nothing else, which
leaves every path below it free to become a link of its own. Tick **dynamic
link** on the create or edit form and the link gets a **pattern** as well: a
template that answers for every path *below* the name, with that path available
as `.Path`.

| | `go/x` goes to | `go/x/sub` goes to |
|---|---|---|
| destination only | the destination | 404, and a create form for `x/sub` |
| destination and pattern | the destination | the pattern, with `.Path` = `sub` |
| pattern only | the pattern, with `.Path` empty | the pattern, with `.Path` = `sub` |

Patterns are [Go templates](https://pkg.go.dev/text/template). Destinations are
not — a destination with `{{` in it is refused, because nothing would expand it.

A query string on the go link is **merged into whatever it resolves to**, pattern
or not, so `go/search/pangolins?hl=nl` reaches the destination with `hl=nl` still
attached. A pattern has no `.Query`; this is what replaces needing one.

Available in a pattern:

- `.Path` — everything after the name, without a leading slash
- `.Now` — a `time.Time` of now
- `.User` — the email of whoever is following the link
- `PathEscape`, `QueryEscape`, `TrimPrefix`, `TrimSuffix`, `ToLower`, `ToUpper`,
  `Match` — `url.PathEscape`, `url.QueryEscape`, `strings.*` and
  `regexp.MatchString`

Shapes worth copying (`go/code` and `go/jira` are real, the rest are the idea):

| what it does | name | destination | pattern |
|---|---|---|---|
| search from the path | `go/code` | `https://github.com/wetravel-com` | `https://github.com/search?q=org%3Awetravel-com+{{QueryEscape .Path}}&type=code` |
| path into the URL path | `go/slack` | `https://wetravel.slack.com/` | `https://wetravel.slack.com/channels/{{PathEscape .Path}}` |
| a Jira ticket | `go/jira` | `https://wetravel.atlassian.net/jira/for-you` | `https://wetravel.atlassian.net/browse/{{.Path}}` |
| today's page | `go/today` | | `http://wiki/{{.Now.Format "01-02-2006"}}` |
| one name only | `go/team` | `https://wiki/team` | |

**Bare Jira tickets are an nginx redirect**, not a link: `go/WTRAV-1234` and
`go/wtrav-1234` are recognised by shape and sent to `go/jira/WTRAV-1234`, where
the `go/jira` link above takes over. The project keys are listed in
`deploy/nginx/patterns.conf`; adding one is a line there and a deploy. Everything
with a name in front of it belongs in golink instead, where anyone can edit it.

### Special URLs

Every URL beginning with a dot is golink itself rather than a link.

| | |
|---|---|
| `go/` | the create form, and the most visited links |
| `go/.all` | every link, with a search box and an edit link on each row |
| `go/.search?q=text` | links whose name, destination or pattern contains `text` |
| `go/.search?q=owner:amelie@wetravel.com` | links owned by one person |
| `go/.detail/name` | a link's details, where it is edited or deleted |
| `go/name+` | the same page, for typing in a hurry |
| `go/.help` | all of this, in the app |
| `go/.export` | every link as JSON Lines — the backup format |
| `go/.export-stats` | click counts as CSV |
| `go/.metrics` | Prometheus metrics |
| `go/.opensearch` | the OpenSearch description, for browser search keywords |

`/.export` can be held to admins with `-admin-only-export`, which also stops the
pages offering the bulk URLs to anyone else. `/.export-stats` and `/.metrics`
keep answering everybody either way, because a metrics scraper holds no session;
that is not a control, so keep them behind whatever authenticates the rest.

## Permissions

**Run with `-open-links`.** Anyone may then create, edit or delete any link, and
editing someone's link does not take it from them. Without the flag you get
upstream's model, where a link belongs to whoever made it.

**Locking is an admin decision.** An admin can lock a link from its detail page,
which takes it out of the open model: only its owner and admins can then change
it. `-owner-can-lock` extends that to the link's owner as well. Without
`-open-links` nobody can lock anything, because a lock would say nothing that is
not already true.

### Admins

Admin rights come from an `Admins` table that golink only ever **reads** — there
is no UI for it, and no flag. A row names either a login or a group, written with
a `group:` prefix:

```sh
# SQLite
sqlite3 data/golink.db "INSERT INTO Admins (Name) VALUES ('amelie@wetravel.com');"
sqlite3 data/golink.db "INSERT INTO Admins (Name) VALUES ('group:eng@wetravel.com');"
sqlite3 data/golink.db "SELECT Name FROM Admins;"
sqlite3 data/golink.db "DELETE FROM Admins WHERE Name = 'amelie@wetravel.com';"

# MySQL, in the local stack
podman compose -f compose.yaml -f compose.mysql.yaml exec mysql \
    mysql -ugolink -pgolink golink -e "INSERT INTO Admins (Name) VALUES ('amelie@wetravel.com');"
```

In the cluster the golink image is distroless — no shell and no client — so use
a one-off pod, or the `mysql` client from anywhere that can reach the database.

- Names match **case-insensitively**, and every request consults the table, so a
  change takes effect immediately with no restart.
- A `group:` row only matches if something is telling golink about groups —
  `-auth-groups-header`, fed by oauth2-proxy. With the plain Google provider that
  header is empty (see [Groups](#groups)), so use user rows until the Admin SDK
  is set up.
- The table rejects a name with a colon in it that does not start with `group:`,
  which catches a misspelled prefix that would otherwise sit there matching
  nobody.
- **It is not in `/.export`.** Back it up separately.

## Creating links from a script

golink has no API keys and no tokens. A form post carries an XSRF token, which a
script cannot mint — but **a request carrying the header `Sec-Golink` skips that
check**, because browser JavaScript is not allowed to set a header by that name,
so it cannot be used for the attack the token exists to stop.

Against the local stack, where nginx supplies a fixed identity, that is all it
takes:

```sh
# create
curl -sf -X POST http://localhost/ -H 'Sec-Golink: 1' \
    --data-urlencode short=onboarding \
    --data-urlencode long=https://wetravel.atlassian.net/wiki/spaces/EG/overview

# the same call updates it: short is the key
curl -sf -X POST http://localhost/ -H 'Sec-Golink: 1' \
    --data-urlencode short=onboarding \
    --data-urlencode long=https://wetravel.atlassian.net/wiki/spaces/EG/overview \
    --data-urlencode pattern='https://wetravel.atlassian.net/wiki/search?text={{QueryEscape .Path}}'

# read one back as JSON, or every link as JSON Lines
curl -sf http://localhost/.detail/onboarding
curl -sf http://localhost/.export
```

- The fields are `short`, `long` and `pattern`, and that is all a script should
  send. **Do not send `dynamicset` or `lockedset`**: those are the form telling
  golink that its checkbox has spoken, so `dynamicset` without `dynamic` clears
  the pattern, and `lockedset` without `locked` unlocks the link. Omitting both
  leaves the pattern as given and the lock exactly as it was.
- `owner` is optional; under `-open-links` a save that omits it keeps the
  existing owner rather than taking the link over.
- A link's JSON comes from `/.detail/<name>` for any client that does not accept
  `text/html`, which is why plain `curl` gets JSON and a browser gets the page.
- **Deleting is not scriptable.** `/.delete/<name>` always requires a token
  minted for that particular link, `Sec-Golink` or not, deliberately — so
  deletion stays something a person does on the detail page.

> [!IMPORTANT]
> **In production, SSO makes this much heavier.** Everything above works because
> the local stack authenticates nobody. Behind oauth2-proxy a script has no
> session, and it cannot fake one: the identity headers are *set* by nginx, so
> passing `X-Auth-Request-Email` yourself achieves nothing. The options are all
> unpleasant:
>
> - copy oauth2-proxy's session cookie (`_oauth2_proxy` by default) out of a
>   browser and pass it with `curl -b`. It works, it is per-person, and it
>   expires;
> - reach golink directly rather than through nginx — which is exactly what the
>   deployment is built to prevent, and what would let anything in the cluster
>   claim to be anyone;
> - do not script production. Bulk work belongs in the local stack, and
>   `/.export` plus `./import.sh` (or `-snapshot`) is how a set of links then
>   gets to production.
>
> If scripted writes in production ever become a real need, the honest fix is a
> service credential golink checks itself — not a way around the proxy.

Do not reach for `-allow-unknown-users` to make any of this easier: it disables
the XSRF check for every request and lets an unidentified caller save links.

## Running it locally

The local stack is nginx in front of golink, shaped like production but
authenticating nobody: nginx tells golink that every visitor is
`dev@wetravel.com` in the group `infra@wetravel.com`. Change those two headers in
`deploy/nginx/golink-dev.conf` to be somebody else.

```sh
./start-dev.sh          # http://localhost:8080/, loopback only
./start-dev.sh 80       # http://localhost/, so bare go/ links work with a hosts entry
./start-dev.sh --mysql  # the same, with the links in MySQL instead of SQLite
```

Then try `http://localhost:8080/fadsfads` — it should render the create form with
the name filled in.

**Bring the stack back with the script, not with `podman compose restart`.**
Restart starts every container at once, so golink can lose the race to its
database, and nginx resolves golink's address once at startup and will hold a
stale one. The script starts things in order and restarts nginx last, which is
most of why it exists.

### SQLite, the default

Links live in `./data/golink.db`, a directory in the working tree rather than a
named volume — a named volume was once removed by a stray compose invocation,
taking the links with it. It also means `sqlite3 data/golink.db` works from the
host, which is how admins are added locally.

### MySQL

`./start-dev.sh --mysql` adds a MySQL container and points golink at it, using
`compose.mysql.yaml` as an override. Two things to know:

- **The two backends hold different links.** Switching does not carry them
  across; `/.export` and `./import.sh` do.
- Its data is a named volume (`podman compose down -v` throws it away), because a
  MySQL data directory is written from inside the container. `/.export` is the
  backup, not the volume.

```sh
# poke at it
podman compose -f compose.yaml -f compose.mysql.yaml exec mysql \
    mysql -ugolink -pgolink golink -e "SELECT Short, Owner FROM Links LIMIT 5;"
```

### Importing an export

```sh
./import.sh /private/tmp/links_export.jsonl            # into ./data/golink.db
./import.sh --mysql /private/tmp/links_export.jsonl    # into the MySQL container
```

A restore adds links and skips any name that already exists, so it cannot push a
changed destination over a link that is already there. On macOS give a real path:
the podman VM has `/private/tmp`, and `/tmp` is only a symlink to it.

### Without any of the stack

To try flags directly:

```sh
go run ./cmd/golink -dev-listen 127.0.0.1:8080 -open-links -sqlitedb "$PWD/golink.db"
```

In that mode there is **no authentication at all** and every visitor is
`foo@example.com`. `go build` needs no npm: `static/base.css` is a committed
Tailwind build, and only changing a class needs `npm run build:css`.

## Google SSO

golink authenticates nobody itself. In front of it:

```
browser -> nginx -> (subrequest) oauth2-proxy -> Google
                 -> golink
```

nginx asks oauth2-proxy about every request, and **sets** two headers on the one
it forwards to golink: the email and the groups. golink is told their names with
`-auth-email-header X-Auth-Request-Email -auth-groups-header X-Auth-Request-Groups`
and refuses a request that arrives without the email one.

> [!WARNING]
> golink cannot tell a header nginx set from one a client sent. Two things are
> therefore load bearing: nginx must **set** both headers rather than pass
> through what it received (`proxy_set_header` redefines, which is why the config
> is written the way it is), and nothing but nginx may be able to open a
> connection to golink. In the cluster that second part has to be enforced by the
> network, not by convention — see [the open risk](#the-open-risk).
>
> Forgetting `-auth-email-header` entirely is the one failure here that is silent
> rather than closed: every visitor becomes `foo@example.com`, the dev-mode user.
> After deploying, create a link and check its owner.

> [!WARNING]
> **Never add `skip_auth_regex` (or any other auth bypass) for the `/.` paths.**
> It looks like the way to let a metrics scraper in, and it hands out the whole
> link set: `/.export` returns every link in one request and `/.metrics` names
> every link in its labels. Scrape the golink pod directly instead, which is what
> the deployment does.

### The Google OAuth client

In Google Cloud, create an **OAuth 2.0 Client ID** of type *Web application*, and
add every URL where golink will answer to its **authorised redirect URIs**:

```
https://go.wetravel.com/oauth2/callback
http://localhost:8080/oauth2/callback     # for local testing, see below
```

The client ID and secret are what `start-prod.sh` and the deployment want. They
are credentials: keep them in the environment or a Secret, never on a command
line.

### Testing on localhost, without TLS

`start-prod.sh` runs the real arrangement on this machine — nginx, a real
oauth2-proxy against Google, golink — which is the way to settle which header
oauth2-proxy actually sets before trusting the guess in the flags:

```sh
GOLINK_OAUTH_CLIENT_ID=... GOLINK_OAUTH_CLIENT_SECRET=... ./start-prod.sh
GOLINK_OAUTH_CLIENT_ID=... GOLINK_OAUTH_CLIENT_SECRET=... ./start-prod.sh --mysql
```

This works over plain HTTP because **Google allows an `http://` redirect URI for
`localhost` only**. Two consequences, both handled in `compose.prod.yaml`:

- `--cookie-secure=false`, since there is no TLS for the session cookie to
  require. Leave it out in the cluster, where TLS terminates in front.
- the redirect URI must match exactly what the browser used, so use
  `http://localhost:8080/`, not `127.0.0.1`, unless you register that too.
  `GOLINK_REDIRECT_URL` overrides it for any other host, which then needs https.

The script generates a session-cookie secret and an XSRF key when they are unset
and says so; both are single-instance only (see
[Running more than one instance](#running-more-than-one-instance)).

### Groups

`X-Auth-Request-Groups` is empty with the plain Google provider: the ID token
carries no group membership. Filling it means giving oauth2-proxy the Admin SDK
credentials — `--google-service-account-json`, `--google-admin-email` and
`--google-group` — after which `group:` rows in the `Admins` table start working.
Until then, list admins by login.

Groups are read from **every** instance of the header, each split on `,` and
trimmed, because proxies disagree about whether to repeat a header or join it.
They confer nothing by themselves; they are looked up in the `Admins` table.

## Configuration

Any option `-help` lists can be given in a file instead, which is where the
credentials belong:

```sh
golink -config /etc/golink/golink.hujson
```

The file uses the flag names as keys. It is [hujson](https://github.com/tailscale/hujson)
— JSON with comments and trailing commas:

```hujson
{
    "dev-listen": ":9292",
    "sqlitedb": "/home/nonroot/golink.db",
    "open-links": true,
    "auth-email-header": "X-Auth-Request-Email",
    "auth-groups-header": "X-Auth-Request-Groups",
    // "mysql": "golink:password@tcp(mysql:3306)/golink",
    // "xsrf-key": "at least sixteen characters of random",
}
```

- **An option on the command line wins over the file**, so a file of settled
  defaults and a one-off override work together.
- **A name the flags do not know is a startup error**, not something ignored.
- `deploy/golink.hujson` is a worked example with the reasoning beside each
  option.

The options this fork adds, all of which default to upstream's behaviour:

| flag | what it does |
|---|---|
| `-open-links` | anyone may edit any unlocked link |
| `-owner-can-lock` | an owner may lock their own link, not only an admin |
| `-auth-email-header` | take identity from this header instead of a tailnet |
| `-auth-groups-header` | groups for `Admins` lookups |
| `-mysql` | store links in MySQL rather than a SQLite file |
| `-xsrf-key` | sign form tokens with a shared secret |
| `-healthcheck-path` | answer this path with the health of the instance |
| `-admin-only-export` | hold `/.export` to admins |
| `-config` | read any of these from a file |
| `-webhook-url`, `-webhook-format` | post audit events somewhere |

Environment defaults: `GOLINK_MYSQL_DSN` for `-mysql`, `GOLINK_XSRF_KEY` for
`-xsrf-key`.

Three of upstream's flags are worth knowing as well:

| flag | what it does |
|---|---|
| `-readonly` | refuse every edit: saves and deletes answer 405, the pages still serve |
| `-snapshot FILE` | restore an export at startup, adding links and skipping names that exist |
| `-allow-unknown-users` | let a caller with no identity save links, **and disable the XSRF check** — for a private single-user instance, not for anything with a proxy in front |

## Running more than one instance

Several golink processes can serve the same links, provided:

1. **The database is one they can share.** Two processes cannot use one SQLite
   file — the second to start fails with `database is locked`. Use `-mysql`.
2. **Every instance has the same `-xsrf-key`**, at least 16 characters. Without
   it each invents its own and refuses the forms rendered by the others, so
   roughly half of all saves fail with `invalid XSRF token` and a retry sometimes
   works. A key shorter than 16 characters is refused at startup rather than
   accepted as a weak one.
3. **Every oauth2-proxy has the same `--cookie-secret`.** Its session lives in
   the cookie, so no sticky sessions and no shared cache are needed — but a
   session minted by one instance has to be readable by the next.

Click counts converge rather than being exact: each instance counts in memory,
writes what it counted once a minute, then reads the totals back, so a link's
count includes every instance's clicks within a minute. `SIGINT` and `SIGTERM`
flush once before exiting, so a rolling restart does not lose the last minute;
a `SIGKILL` does.

## Health checks

`-healthcheck-path /healthcheck` answers that one path with the health of the
instance instead of treating it as a link name: 200 when the database answers,
503 when it does not, so a load balancer stops sending requests to an instance
that cannot serve them. The body says nothing else — the reason goes to the log,
not to whoever asked.

It is a flag rather than a fixed route because the path it takes stops working as
a link name, and golink says so at startup. Unset, there is no such endpoint.

## Audit log and webhook

Every create, update and delete writes one JSON object to **stdout**, with the
Datadog reserved attributes (`status`, `service`, `message`) so it arrives as a
parsed event:

```json
{"timestamp":"2026-09-04T12:25:46Z","status":"info","service":"golink",
 "message":"update go/code by amelie@wetravel.com","action":"update","short":"code",
 "user":"amelie@wetravel.com",
 "link":{"long":"https://after.example.com/","owner":"amelie@wetravel.com"},
 "previous":{"long":"https://before.example.com/","owner":"amelie@wetravel.com"}}
```

Operational logging goes to stderr, so a parser never has to tell the two apart.
`podman compose logs golink` is the local version of what a collector ships.

`-webhook-url` also posts each event, shaped by `-webhook-format`: `slack` for a
message an incoming webhook renders, or `json` for the event itself. Saving a
link never waits on it and never fails because of it — events go to a buffered
channel that one goroutine drains, a full queue drops them with a line saying so,
and a failure is logged without the URL, which is a credential.

**Not audited**: restoring a snapshot, and anything done to the database by hand,
including every change to `Admins`.

## Backups

`/.export` is every link as JSON Lines, and the only thing that moves links
between backends:

```sh
curl -sf http://localhost/.export > golink-$(date +%F).jsonl
```

Restore with `./import.sh` locally, or `-snapshot` at startup, which adds links
and skips names that already exist. A snapshot can also be read without a server
at all:

```sh
golink -resolve-from-backup golink-2026-09-09.jsonl go/link
```

The `Admins` table and the click counts are **not** in an export; dump them
separately if they matter.

## Data model

Three tables, in both backends, created if missing on every start. There is no
migration step: a column added later has to be applied by hand.

| table | what it holds |
|---|---|
| `Links` | `ID` (the normalised name), `Short`, `Long`, `Pattern`, `Created`, `LastEdit`, `LastEditBy`, `Owner`, `Locked` |
| `Admins` | `Name` (a login, or `group:` and a group), `Created`. Read-only to golink |
| `Stats` | `ID`, `Created`, `Clicks` |

Two things to know before querying by hand:

- **`Stats` is an append-only ledger, not a counter.** Nothing updates a row.
  Once a minute, each instance inserts one row per link that was clicked in that
  minute, holding the clicks *for that minute only* — so a link has as many rows
  as it has had minutes with a click in them, and its total is their sum:

  ```sh
  sqlite3 data/golink.db "SELECT ID, count(*) AS rows, sum(Clicks) AS clicks
      FROM Stats GROUP BY ID ORDER BY clicks DESC LIMIT 5;"
  ```

  Two instances therefore both insert their own rows for the same minute and the
  sum stays right. Nothing prunes the table; deleting a link deletes its rows.

- **`ID` is the resolved form of the name**, lower-cased with hyphens removed, so
  `meeting-notes` is stored with `ID` `meetingnotes`. Look links up by `ID` the
  way golink does, or by `Short` for what somebody typed.

Times are Unix seconds. Editing `Links` by hand works and is sometimes the
quickest fix, but nothing done that way appears in the audit log.

## Deploying to Kubernetes

Three apps make one service, and they are three only because the deployment chart
gives an app one image:

```
ALB (internal) -> wt-golink-nginx -> (subrequest) wt-oauth2-proxy -> Google
                                  -> wt-golink -> RDS MySQL
```

### 1. The images

| app | image | built from |
|---|---|---|
| `wt-golink` | `golink` | `Dockerfile` in this repo |
| `wt-golink-nginx` | `golink_nginx` | `deploy/nginx/Dockerfile` in this repo |
| `wt-oauth2-proxy` | `docker_base_images/oauth2-proxy` | upstream's, mirrored into ECR |

The nginx config is **baked into the image** rather than mounted from a
ConfigMap, so one tag names one behaviour: a rollout carries the config with it
and a rollback takes it back. A config change is therefore a build.

```sh
podman build -t golink        -f Dockerfile .
podman build -t golink_nginx  -f deploy/nginx/Dockerfile deploy/nginx
```

`nginx -t` cannot run at build time, because the config names Services that do
not resolve there. Check the image by running it:

```sh
podman run --rm -d --name nginx-check -p 8082:9292 --read-only --tmpfs /tmp \
    --add-host wt-golink:127.0.0.1 --add-host wt-oauth2-proxy:127.0.0.1 golink_nginx
curl -sf http://localhost:8082/healthcheck                      # 200 ok
curl -sI http://localhost:8082/WTRAV-1 | grep -i location       # /jira/WTRAV-1
```

### 2. The manifests, which are Helm values

The Kubernetes resources live in **`wetravel-com/argo-gitops`**, not in this repo,
and there are no manifests to write: that repo has a shared `helm/wt-service`
chart, so an app is a values file plus one element in the production
ApplicationSet.

```
helm/values/wt_golink/{base,production}.yaml
helm/values/wt_golink_nginx/{base,production}.yaml
helm/values/wt_oauth2_proxy/{base,production}.yaml
appsets/production.yaml          # - appName: wt-golink / valuesDir: wt_golink
```

The golink app, cut down to what matters:

```yaml
appName: wt-golink
imageName: golink
imageTag: master-1234abcd        # patched by CI after each build
technology: go
components:
  web:
    replicas: 2
    hasHttp: true
    hasHealthcheck: true
    hasIngress: false            # only nginx may reach it
    port: 9292
    command:
      - /golink
      - -dev-listen=:9292
      - -open-links
      - -admin-only-export
      - -healthcheck-path=/healthcheck
      - -auth-email-header=X-Auth-Request-Email
      - -auth-groups-header=X-Auth-Request-Groups
envVars:
  GOLINK_XSRF_KEY: "@secret/golink/xsrfKey"
  MYSQL_HOST: "@secret/mysql-users/host"
  MYSQL_PASSWORD: "@secret/mysql-users/golink_prod"
  GOLINK_MYSQL_DSN: "golink_prod:$(MYSQL_PASSWORD)@tcp($(MYSQL_HOST):3306)/golink"
```

Values are layered `common/production.yaml` → `<app>/base.yaml` →
`<app>/production.yaml`, last wins. Render before pushing:

```sh
helm template test helm/wt-service -f helm/values/common/production.yaml \
    -f helm/values/wt_golink/base.yaml -f helm/values/wt_golink/production.yaml \
    --set release=production
```

### 3. What has to exist first

In **`wetravel-com/infrastructure`**, because the values only *reference* these:

- ECR repositories for `golink` and `golink_nginx`
- the `golink` database and a `golink_prod` user on the production RDS instance
- the Secret keys: `golink/xsrfKey`, `golink/cookieSecret`,
  `golink/oauthClientId`, `golink/oauthClientSecret`, and
  `mysql-users/golink_prod`
- DNS for `go.wetravel.com`, and whatever makes bare `go/` resolve for staff
  (a search domain, or a short-name record)
- this repo registered in `github/`, so it gets the standard `build_and_test.yml`
  — which is also what patches `imageTag` in argo-gitops after a build

`GOLINK_XSRF_KEY` and oauth2-proxy's `--cookie-secret` must be **the same value
in every pod**. There is no schema migration: the tables are created if missing
but a column added later has to be applied by hand, and against RDS that is a
change no image rollback undoes.

### 4. Four things the cluster forces

- **Everything listens on 9292.** The platform's NetworkPolicy allows pod-to-pod
  traffic on 80 and 9292 and nothing else, so a golink on 8080 is unreachable
  from nginx.
- **`/healthcheck` has to exist**, and by that name: the chart's probes default
  to it and the Datadog service check it annotates every Service with cannot be
  told another. nginx answers its own, *before* the auth subrequest — a health
  check that got a 302 to Google would never report healthy. oauth2-proxy is
  given `--ping-path=/healthcheck` for the same reason.
- **nginx resolves the golink Service once, at startup**, and refuses to start at
  all if the name does not exist. Deploying it first crashloops until the Service
  appears; it recovers by itself.
- **TLS ends at the ALB**, so nginx sees plain HTTP. `forwarded.conf` takes the
  scheme from `X-Forwarded-Proto`, and `absolute_redirect off` keeps the pattern
  redirects relative — otherwise nginx builds them from the port it listens on
  and sends people to `http://host:9292/...`.

### The open risk

golink cannot tell an identity header nginx set from one a client sent, and the
chart's NetworkPolicy lets **any pod in the cluster** reach a serving port
("authorization is the mesh's job"). `hasIngress: false` keeps golink off the
internet, which is the larger half, but cluster-internally a compromised pod
could act as any user and read every link. Closing it means one of: hand-written
policies with `networkPolicy.enabled: false`, a Linkerd `AuthorizationPolicy`, or
a shared secret header that nginx sets and golink requires — the same shape as
the `INTERNAL_API_KEY` other services here use. None of those is in place;
`CLAUDE.md` has the detail.

## Browser configuration

For `go/thing` to work from the address bar, `go` has to resolve — a DNS search
domain, a short-name record, or a `hosts` entry pointing at the local stack:

```
127.0.0.1 go        # with ./start-dev.sh 80
```

In Firefox, two settings help:

- to stop `go/` in the address bar being treated as a search, set
  `browser.fixup.domainwhitelist.go` to *true* in `about:config`
- if you use HTTPS-Only Mode,
  [add an exception](https://support.mozilla.org/en-US/kb/https-only-prefs#w_add-exceptions-for-http-websites-when-youre-in-https-only-mode)

`go/.opensearch` also lets a browser treat `go` as a search keyword.

## Running on a tailnet

Upstream's tailnet mode is still in the binary and still works: leave
`-dev-listen` unset, give it a `TS_AUTHKEY`, and it joins a tailnet, serves on
`http://go/`, and takes identity and admin rights from there. Nothing in this
fork's deployment uses it, and the flags above are what replace it. For that
mode, read [upstream's README](https://github.com/tailscale/golink#readme) — its
`-advertise-tags`, `-register-as-service`, `-https` and MagicDNS notes all still
apply.

## Licence

BSD-3-Clause, as upstream. `LICENSE` and the copyright headers stay on every
file, including the ones this fork rewrote. What was removed is Tailscale's
*branding* from the pages, which a copyright licence does not grant in the first
place; the footer keeps a plain-text "Built on golink" link as attribution.
