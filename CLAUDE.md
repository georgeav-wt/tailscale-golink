# golink fork — WeTravel go/ links

This is a fork of [tailscale/golink](https://github.com/tailscale/golink) (BSD-3-Clause),
modified to run **without a tailnet**, behind nginx + oauth2-proxy, with an
**open-by-default** link permission model.

Upstream is ~1,611 lines of Go across 3 files (`golink.go`, `db.go`, `cmd/golink/main.go`)
and changes slowly. **Keep this fork's diff minimal and localised so rebasing stays cheap.**

---

## Why this fork exists

WeTravel needs `go/thing` internal shortlinks. Requirements:

1. **Create-on-404** — hitting `go/doesnotexist` must drop you into a create form with
   the name pre-filled. This is the defining requirement; it eliminated most candidates.
2. **Open by default, lockable** — anyone can create/edit any link; an owner may *lock*
   a link so only they (or an admin) can change it; an admin can always override.
3. **Google SSO** — handled by oauth2-proxy in front, NOT in the app.
4. **Dynamic links** — `go/JIRA-1234`, `go/pr/1234`. Split between nginx and golink templates.
5. Self-hosted on EKS. MySQL was preferred but is not available in any viable option.

golink was chosen because three of these already exist upstream and the fourth is ~40 lines.

## Options evaluated and rejected (do not re-litigate)

| Option | Why rejected |
|---|---|
| YOURLS | No create-on-404 — `yourls-loader.php` 302s to the homepage, keyword discarded |
| Shlink | No create-on-404; API-key auth only, no users; SSO closed as "not planned" |
| Kutt | No create-on-404 (`res.redirect(domain?.homepage \|\| "/404")`); links are per-user |
| Trotto | Has create-on-404, but OSS is **sunset** (final commit Apr 2025, branch `no-more-updates`) |
| go-shorten | Has create-on-404 + regex storage, but `Storage` interface is a bare `short → url` string map — adding owner/lock means changing 5 backends. One maintainer |
| kellegous/go | Has create-on-404, but 4,331 lines / 41 files, mandatory npm+Vite build, no identity model, v2 rewrite in flight |
| crhuber/golinks | 404 goes to a search page, not a create form; no auth at all |
| mkende/golink-url-shortener | Has literally every requirement already — **read it as a reference implementation** — but 0 stars, single author, self-declared AI-written |
| GoLinks / Trotto SaaS | ~$12k–18k/yr at 500 users. Viable fallback if this fork stalls |

## Upstream contribution posture

Tailscale accepts outside PRs readily (~21% of commits; median merge ~1.5 days for small
changes). **No CLA; DCO `Signed-off-by` required** (`git commit -s`).

**But this specific change has poor precedent.** PR #231 implemented Google IAP header auth
as an alternative to `WhoIs()` — zero maintainer comments, author closed it the same day.
Issue #166 ("does this work without tailscale?") was closed with no reply. The README points
non-Tailscale users at golinks.io/trot.to. Maintainers are explicitly conservative about
trusting headers (PR #215: header-trust flagged as blocking).

**Assume we maintain this fork indefinitely.** If we do try upstreaming, file an issue first
proposing a *pluggable `currentUser` provider* — widening the seam that already exists — and
get design ack before writing code. Never open a speculative large PR here.

The `-open-links` work (step 1) is the part that could plausibly be offered: it is
opt-in, default-off, and touches no identity code. Step 3 is the part with the bad
precedent. Keep the steps in separate commits so the first can be sent on its own.

---

## Upstream facts verified by reading the source

Do not re-derive these.

**`canEditLink` (golink.go ~1186)** is already most of our permission model:

```go
func canEditLink(ctx context.Context, link *Link, u user) bool {
	if *readonly { return false }
	if link == nil || link.Owner == "" { return true }   // new or unowned
	if u.isAdmin || link.Owner == u.login { return true } // admin or owner
	owned, err := userExists(ctx, link.Owner)
	return err == nil && !owned                           // orphaned owner -> anyone
}
```

Admin override exists. Owner-edits-own exists. Orphaned-owner-becomes-editable exists.
Only the **default polarity** is wrong: upstream is locked-to-owner, we want open-with-opt-in-lock.

**`currentUser` (golink.go ~904)** is a package-level function *variable* — the clean seam
for injecting proxy identity:

```go
var currentUser = func(r *http.Request) (user, error) {
	if devMode() { return user{login: "foo@example.com"}, nil }
	...
}
```

The `user` struct already has `login` and `isAdmin`.

**`trustIdentityHeaders`** already reads `Tailscale-User-Login`, but is hard-gated to tsnet
Service mode + loopback client. Useful as a model, not directly reusable.

**`userExists()`** calls the tailnet API (`localClient.Status`). In dev mode it always returns
true. Once we move to an explicit `Locked` flag we largely stop needing it.

**`schema.sql`:**
```sql
CREATE TABLE IF NOT EXISTS Links (
	ID TEXT PRIMARY KEY, Short TEXT, Long TEXT,
	Created INTEGER, LastEdit INTEGER, Owner TEXT
);
CREATE TABLE IF NOT EXISTS Stats (ID TEXT, Created INTEGER, Clicks INTEGER);
```

**Create-on-404 already works** (golink.go ~671): `w.WriteHeader(404)` then `serveHome(w, r, short)`;
`tmpl/home.html` renders `value="{{.Short}}"` with autofocus on the destination field.

**Build:** `static/base.css` is committed, so `go build` needs **no npm**. npm/tailwind is only
needed if Tailwind classes change (`npm run build:css`).

**Flags:** `-dev-listen ADDR` (plain `http.ListenAndServe`, no tsnet, no auth),
`-sqlitedb PATH` (**required** — without it dev mode uses a temp dir and loses data on restart),
`-readonly`, `-allow-unknown-users`, `-snapshot`, `-admin` via tailnet ACL grants.

**Destination templates:** Go `text/template` with `.Path`, `.Now`, `.User`, `.Query`,
plus `PathEscape` / `QueryEscape`. Short names are single-segment; `.Path` is everything after.

---

## Implementation plan

### 1. `Locked` column — invert the permission default (**done**, commit `golink,db: add -open-links, and make locking a link an admin decision`)

Implemented behind a **`-open-links` flag, off by default**, so that a golink built
from this fork behaves exactly like upstream unless the flag is passed. That keeps
the diff a strict addition rather than a change of semantics, which is the only
version of this that has any chance upstream. **We run with `-open-links`.**

- `schema.sql`: `Locked INTEGER NOT NULL DEFAULT 0`. There is deliberately **no
  migration code**; see the note under step 16 for what that means the day a column is
  added after deploying.
- `db.go`: `Locked bool` on `Link`, tagged `json:",omitempty"` so `/.export` snapshots
  of unlocked links stay byte-identical to upstream's.
- `golink.go`: `canEditLink` keeps the upstream path (owner, admin, or orphaned owner)
  and adds `if *openLinks { return !link.Locked }` before the `userExists` fallback.
  `ownsLink` is the owner-or-admin test, and is what lets an owner go on **editing**
  a link that is locked.
- **Locking is an admin decision** — `canLockLink` is `*openLinks && (isAdmin ||
  *ownerCanLock && owner)`. A lock takes a link out of the model every other link is
  in, so it does not belong to whoever created it first; `-owner-can-lock` hands it to
  owners as well if that turns out to be too strict. Without `-open-links` nobody can
  lock, because a lock would say nothing that is not already true.
  An owner may still edit a link an admin locked; the lock keeps everyone *else* out.
- `tmpl/detail.html`: a "Lock this link" checkbox, rendered only when `.Lockable`
  (`-open-links` and the viewer may lock). An owner who cannot unlock is told the link
  is locked instead, rather than being shown nothing.
- `serveSave`: two subtleties, both load-bearing —
  - the lock only changes when the form posts `lockedset`, because an unchecked
    checkbox submits nothing; without the marker any API client that has never
    heard of locks would silently unlock a link on every update.
  - the permission is checked where the owner is known, just before the save, and
    against the owner the link has **now**: transferring a link away does not hand over
    the right to lock it on the way out, and a link being created is judged by the
    owner it is about to get, so a non-admin cannot create one already locked.
  - under `-open-links`, a save with no `owner` field keeps the existing owner
    instead of assigning the link to the saver. Otherwise editing someone's link
    takes it from them, along with their exclusive right to lock it. Upstream's
    behaviour (the saver becomes the owner, which is how an orphaned link is
    claimed) is unchanged when the flag is off.

### 2. Admins table (**done**, commit `golink,db: read admins from an Admins table`)

Admin rights come from a tailnet ACL grant upstream, which we do not have. Add an
`Admins (Name, Created)` table that golink only ever **reads**; the operator manages
rows with `sqlite3`. A row names a login, or a group with a `group:` prefix, matching
the vocabulary tailnet ACLs already use. This settles the open question from step 3 —
admin status comes from the table, not from a flag, and the `X-Auth-Request-Groups`
header feeds the table rather than deciding anything itself.

```sh
sqlite3 golink.db "INSERT INTO Admins (Name) VALUES ('george.avramoiu@wetravel.com');"
sqlite3 golink.db "INSERT INTO Admins (Name) VALUES ('group:eng@wetravel.com');"
```

- `schema.sql` uses `CREATE TABLE IF NOT EXISTS`, which does create a *missing table*
  (unlike a missing column), so existing databases need no migration for this.
  The table is only ever edited by hand, so SQLite does what checking it can:
  `COLLATE NOCASE` collapses the same name typed in two cases into one row, and a
  `CHECK` requiring any name with a colon in it to start with `group:` catches a
  misspelled prefix that would otherwise sit there matching nobody. Logins never
  contain a colon, so nothing legitimate trips it.
- `db.go`: `IsAdmin(login, groups)` builds the list of names that would make this
  user an admin — their login, plus `group:` + each of their groups — and asks for
  one `IN` match. Prefixing at lookup time is what keeps a group and a user of the
  same name (Google groups *are* email addresses) from being confused for each other.
- `golink.go`: `requestUser` wraps the `currentUser` var and ORs in the table lookup;
  the five handlers call it instead. Keeping the wrapper separate leaves `currentUser`
  as the untouched upstream seam that step 3 replaces.
- No caching: one indexed lookup per request against a tiny table, and it means
  `sqlite3` edits take effect with no restart. Verified against a running server.
- The table is **not** in `/.export`; back it up separately.

**`user.groups` is populated by nothing until step 3.** WhoIs reports no groups, so
`Kind='group'` rows match nobody yet. The plumbing and its tests are in place, so
step 3 is only `u.groups = strings.Split(r.Header.Get("X-Auth-Request-Groups"), ",")`.

### 3. Proxy identity — replace `currentUser` (**done**, commit `golink: identify users by proxy-set headers`)

`-auth-email-header X-Auth-Request-Email -auth-groups-header X-Auth-Request-Groups`.
Naming the headers rather than hardcoding them costs nothing and keeps the door open
for IAP or Authelia, which use different ones.

- `proxyUser` reads the headers; `Run` assigns it to the `currentUser` **var** when
  `-auth-email-header` is set. Upstream's `currentUser` body is therefore untouched,
  which is the whole point of the seam described above — the diff is an addition, not
  an edit, and rebases cannot conflict inside it.
- The email header must precede the `devMode()` short-circuit, and it does, because
  we replace the var rather than adding a branch inside it. We run with `-dev-listen`,
  so the hardcoded `foo@example.com` would otherwise win. Verified: the owner of a
  link created through the proxy is the header's email, not `foo@example.com`.
- Groups are read from **every** instance of the header, each split on `,`, trimmed,
  empties dropped — proxies disagree on whether to repeat the header or join it.
  They decide nothing by themselves; they are looked up in the `Admins` table.
- **oauth2-proxy only knows Google Groups via the Admin SDK.** The plain Google
  provider gets no groups from the ID token, so `X-Auth-Request-Groups` is empty
  unless oauth2-proxy is given `--google-service-account-json`, `--google-admin-email`
  and `--google-group`. Until that is set up, use user rows in `Admins`, not
  `group:` rows.
- A missing email header is an error, which handlers turn into a 500 — upstream's
  mapping for a `currentUser` failure. It is a deployment fault, not a user one, so
  that is about right. `-allow-unknown-users` still overrides it, as upstream.

**Security note:** in `-dev-listen` mode the app has *no* authentication of its own. The
proxy is the only control. The deployment must guarantee nothing can reach the golink pod
directly — NetworkPolicy restricting ingress to the nginx pod is mandatory, not optional.
nginx must also **set** both headers on every proxied request rather than passing through
what a client sent; oauth2-proxy in front of it does this, but an `auth_request` setup
that only *adds* headers can leave a client-supplied one in place.

### 4. Names with slashes, and a pattern beside the destination (**done**, commit `golink,db: allow slashes in names, and give a link a pattern`)

Two changes that arrived together, because the second is what makes the first useful:
a name can be a path, and a link says separately where its own name goes and what to do
with a path below it.

**Names may contain slashes.** `go/team`, `go/team/jira` and `go/team/github` are
independent links, each with its own owner, lock and stats. `reShortName` is
`^\w[\w\-\.]*(/\w[\w\-\.]*)*$`; a word character at the start of every segment keeps a
name clear of the internal `/.x` routes and rejects leading and trailing slashes and
empty segments. The same pattern is in `home.html`, `detail.html` and `delete.html`.
Create-on-404 pre-fills the whole path, and `{name}+` matches the whole path.

**A link has two fields.** `Long` is the destination, used exactly as written.
`Pattern` is a template expanded for a path *below* the name, which reaches it as
`.Path`. Upstream had one field that was both, which is why a plain `go/team` used to
answer for `go/team/newthing` instead of offering to create it, and why every dynamic
link needed `{{if .Path}}` to serve its own bare name.

| | destination only | destination + pattern | pattern only |
|---|---|---|---|
| `go/x` | the destination | the destination | pattern, `.Path` empty |
| `go/x/sub` | 404 → create form | pattern, `.Path=sub` | pattern, `.Path=sub` |

`go/code` is now two plain fields, and the conditional is gone:

```
long:    https://github.com/wetravel-com
pattern: https://github.com/search?q=org%3Awetravel-com+{{QueryEscape .Path}}&type=code
```

- **There is no dynamic flag.** A pattern being present *is* what makes a link answer
  for the paths below it, so the two can never disagree. An intermediate commit did
  have a `Dynamic` bool; it is gone.
- `lookupLink` is the whole resolution rule: the exact name, else the longest prefix of
  the path that has a pattern. A shorter pattern still wins over a longer name with no
  pattern. `serveGo` and `resolveLink` (the `-resolve-from-backup` CLI) share it.
- `resolveTarget` picks between the two fields; the query merging both need came out of
  `expandLink` into `mergeQuery`.
- `serveSave` rejects `{{` in the destination, since it is never expanded — **except**
  that a template arriving in `long` with no `pattern` is moved to `pattern`, which is
  what a client written before this field sends. `legacyPattern` does that conversion
  and is shared with `UnmarshalJSON`, so both agree.
- Two compatibility hinges, both tested:
  - `Link.UnmarshalJSON` reads a snapshot from any of the three eras: `Pattern` present
    is taken as written, otherwise `Dynamic` decides, and absent that, every link was
    dynamic. This is why `Pattern` is exported even when empty — an omitted field has
    to keep meaning "written before patterns".
  - `-resolve-from-backup` resolves through the same `lookupLink`, so a snapshot and a
    live database answer identically.
- **Not upstreamable as-is**, unlike steps 1-3: it changes what an existing link does
  and adds a field to the export. Offering it would mean a flag for the default.

### 5. Searching and editing from the link list (**done**, commit `golink,db: search links by name and destination`)

- `/.all` and `/.search` share `search.html`, which now carries a search box and gives
  every row a visible **Edit** link. The link was there before as an icon revealed on
  hover, which no touch device can reveal.
- `/.search?q=` keeps `owner:<email>` and treats anything else as a substring of the
  short name, destination or pattern. Server-side, so there is no JavaScript on any
  page; the whole app stays server-rendered HTML.
- The search also matches the normalised ID, so `hibob` finds `hi-bob` the same way
  resolution does. `containsPattern` escapes `%` and `_` so a query means itself.
- `searchTmpl` now takes a `searchData{Query, Results}` rather than a bare slice, so
  the box can show the current query.
- The four queries that read links share `scanLink`/`scanLinks` now. Three copies of
  that loop had already gone wrong twice while adding columns in this fork.

### 6. De-branding, the create form, and the special links (**done**, commit `tmpl: drop Tailscale's branding, and tidy the pages`)

**Licensing.** golink is BSD-3-Clause. Using and modifying it inside WeTravel without
publishing anything is fine; the licence only asks that a *redistribution* keep the
notices. So `LICENSE` and the `// Copyright 2022 Tailscale Inc & Contributors` headers
**stay**, on every file, including the ones this fork rewrites. What went is branding,
which is a different thing from attribution and is not licensed by BSD-3 at all:
clause 3 bars using the copyright holder's name to promote a derived product, and
trademarks are not granted by a copyright licence. The footer keeps a plain-text
"Built on golink" link, which is factual rather than promotional; delete it if even
that is unwanted.

Also gone are three claims that are false here: "for your tailnet" in the header and
the OpenSearch description, "when logged in to your Tailscale network" on the help
page, and the note about `{username}@github` logins.

**The special links are documented.** The help page now lists the URLs that are golink
itself rather than links -- `/.all`, `/.search`, `/.detail/`, `/.export`, `/.metrics`
and the rest -- with what each is for, and the link list has a way back to the home
page. The `go/` in the header is a link and now looks like one.

**The create form.** Most links are not dynamic, so the pattern fields now sit behind
a "dynamic link" checkbox, on the create and the edit form alike; the edit form ticks
it when the link has a pattern.

- The reveal is **CSS only** — `.dynamic-toggle:checked ~ .dynamic-fields` in a `<style>`
  block in `base.html`. There is no JavaScript anywhere in this app and this did not
  add any. It lives in `base.html` rather than `base.css` because that file is
  generated by Tailwind, and `peer-checked:` is not in the committed build; using it
  would make `npm install` a build dependency, which the fork deliberately avoids.
- The checkbox and the fields it reveals **must stay siblings**, in that order, or the
  `~` selector stops matching. Keep them that way when editing either form.
- A hidden field is still submitted, so `serveSave` treats an unticked box as "no
  pattern" no matter what the field holds. The marker is `dynamicset`, as with the
  lock. When it is absent the legacy shim runs instead, which is what keeps API
  clients that put a template in `long` working: a form that has spoken is never
  second-guessed by the shim.

### 7. Audit log and last editor (**done**, commit `golink,db: record who last edited a link, and log every change`)

**`LastEditBy` on a link**, shown beside the last-edited date and in the JSON. Empty for
links last saved before the column existed; that is not recoverable, so it is simply
empty rather than guessed at.

**An audit log of every create, update and delete**, one JSON object per line on
**stdout**. golink knows nothing about Datadog: it writes a line, and whatever collects
container output ships it. No API key in the app, no network call, nothing to retry.

```json
{"timestamp":"2026-09-04T12:25:46Z","status":"info","service":"golink",
 "message":"update go/audit by dev@wetravel.com","action":"update","short":"audit",
 "user":"dev@wetravel.com",
 "link":{"long":"https://after.example.com/","pattern":"https://after.example.com/{{.Path}}","owner":"dev@wetravel.com"},
 "previous":{"long":"https://before.example.com/","owner":"dev@wetravel.com"}}
```

- `status`, `service` and `message` are Datadog **reserved attributes**, so a line
  arrives as a parsed event with a severity, a service and a readable body rather than
  a wall of text. `source` is set by the collector, not by golink. Whether Datadog reads
  the event time out of `timestamp` or stamps its own on ingest is worth checking in the
  pipeline if the difference ever matters; for a live tail it is under a second.
- Operational logging stays on **stderr**, so the two streams never have to be told
  apart by a parser.
- `previous` appears on an update only, and holds what the link looked like before, so
  "who changed this and to what" is answerable from one line.
- Datadog picks it up either with `containerCollectAll`, or per container:

  ```yaml
  ad.datadoghq.com/golink.logs: '[{"source":"golink","service":"golink"}]'
  ```

- **What is not audited**: restoring a snapshot, and any edit made straight to the
  database with `sqlite3` — including the `Admins` table, which has no audit at all.
  Neither passes through the handler that writes these lines. If the Admins table needs
  an audit trail, that is a separate change.
- `auditWriter` is a package variable so tests can read what was written; `TestAuditLog`
  drives a create, an update by a second user, and a delete through the handlers and
  checks each line.

### 8. Suggestions on a miss, and a sortable list (**done**, commit `golink: suggest near misses, and let the link list be sorted`)

**Kept the UI free of JavaScript and of npm**, which is the constraint worth knowing
before touching a template: `static/base.css` is a prebuilt Tailwind artifact, so a
class that is not already in it does nothing until somebody runs `npm run build:css`.
Check before using one:

```sh
grep -E '^\.text-xs[ ,{:]' static/base.css
```

**`suggestLinks` on a name that does not exist**, above the create form. It scores every
link against the name asked for, on normalised IDs so that case and dashes are ignored
exactly as they are when resolving: a prefix relation either way scores best (`go/gh/inf`
offers `gh`, `gh/infra`, `gh/infrastructure`), then an edit distance of one or two
(`go/hibbob` offers `hibob`), then a sibling under the same first segment, then a
substring. Five at most. It reads every link on a miss, which is one query against a
small table, and only when a name was actually asked for.

**Sortable columns on `/.all` and `/.search`**, as links rather than script:
`sortOrders` holds one comparison per order, each with the direction that is useful for
it (names up, clicks and dates down), and `searchData.SortLink` builds the heading URLs
so a search keeps its query while changing order. An unknown order falls back to name
rather than erroring.

**Badges** in the list for a locked or dynamic link, since both change what can be done
with a link and neither was visible outside its detail page.

Two things not done, offered and declined for now: an empty state on the home page,
which lists only links with recorded clicks and so looks empty on a fresh import; and
rendering the 33 `http.Error` responses in the layout instead of as bare text.

### 9. A destination with no scheme (**done**, commit `golink: assume https for a destination written without a scheme`)

`g.co/test` is a *relative* URL, so a browser resolved it against golink itself and
`go/x` landed on `http://go/g.co/test`. `withScheme` supplies `https://` when a
destination or a pattern was written without one.

- **Two shapes are left alone.** One beginning with `/` aliases another link, which
  `resolveLink` follows on purpose and which upstream's own fixtures use (`chat` → `/meet`).
  One beginning with `{{` decides its own scheme when expanded, and there is nothing to
  inspect before then.
- **A colon is not a scheme.** `localhost:8080/foo` is a host and a port and wants
  `https://`, while `mailto:someone@example.com` is a scheme and must not be touched.
  `hasScheme` tells them apart by what follows the colon: digits to the end or to the
  first `/` are a port. `mailto:2fa@example.com` is a scheme, and is tested.
- **Nothing is silent.** The success page shows what the link now points at and says a
  scheme was added, with a link to edit it. Guessing https is right for anything public;
  the case it gets wrong is an internal box that is http-only, and the fix is to write
  the `http://` out, which `withScheme` then leaves alone.
- Rejecting the save instead was the alternative, and it is worse: creating a link fast
  from a 404 is the point of the tool, and a 400 in the middle of that is a wall.
- It runs at **save** time, so the stored value is what the UI shows and what `/.export`
  carries. Links already stored are untouched; a snapshot restored with `-snapshot` does
  not pass through it either. To find them:

  ```sh
  sqlite3 data/golink.db "SELECT Short, Long FROM Links WHERE Long <> '' AND Long NOT LIKE '%:%' AND Long NOT LIKE '/%';"
  ```

### 10. A configuration file (**done**, commit `golink: read options from a configuration file`)

`-config PATH`. The file names the **same options the command line does**, so anything
`-help` lists is settable in it and a new option needs nothing added to the loader:

```hujson
{
    // Comments and trailing commas are allowed.
    "open-links": true,
    "sqlitedb": "/home/nonroot/golink.db",
    "auth-email-header": "X-Auth-Request-Email",
}
```

- **An option given on the command line wins**, which is what makes a file of settled
  defaults and a one-off override work together. `flag.Visit` reports only the flags
  that were actually passed, so the loader knows which to leave alone.
- **A name the flags do not know is a startup error**, not something ignored. The file is
  where the options are written down; a misspelling that silently did nothing would be
  the worst possible behaviour, and this fork has already been bitten once by a silently
  ignored name (`mt-2` in the committed Tailwind build).
- **Why hujson and not TOML, YAML or plain JSON.** It is already *compiled into the
  binary*: `tailscale.com/ipn/conffile` and `tailscale.com/util/syspolicy/source` both
  import it and both arrive with tsnet, so using it added no download and no linked
  package (`go list -deps ./cmd/golink | grep -c hujson` is 1 either way). TOML and
  YAML appear in `go.sum` but are **not** in the build graph, so either would be a
  genuinely new compiled dependency for one small feature. Plain JSON is free but has
  no comments, and this is a file of security-relevant switches that wants explaining
  beside each one.
  The counter-argument, if it ever comes up again: hujson is a Tailscale library used
  in exactly one place here, so **if this fork ever sheds `tailscale.com` it stops
  being free** and should be swapped for TOML at that point. The format is JWCC, not a
  Tailscale invention, and the loader is 30 lines, so the swap is cheap.
- `loadConfig` takes a `*flag.FlagSet` rather than reaching for the global one, so its
  test drives its own flags and cannot disturb another test's.
- `deploy/golink.hujson` is a worked example, including the note that
  **`webhook-url` is a credential**: prefer the file to the command line, where the URL
  would show up in the process list.

### 11. Webhooks for the audit log (**done**, commit `golink: post audit events to a webhook`)

`-webhook-url` posts every create, update and delete somewhere. `-webhook-format`
is `slack` (a message an incoming webhook renders) or `json` (the audit event itself,
the same object the log line carries).

```
*update* <http://go/.detail/code|go/code> by amelie@wetravel.com
https://github.com/wetravel-com/new
_was_ https://github.com/wetravel-com (pattern https://github.com/search?q=a&amp;type=code)
```

- **Saving a link never waits on Slack and never fails because of it.** Events go to a
  buffered channel that one goroutine drains; the HTTP client has a 10s timeout, and a
  failure is logged and dropped rather than retried. Verified: with the URL pointed at a
  closed port the save still returned 200 and the link resolved.
- **A full queue drops events**, with a line saying so, rather than growing without
  bound. An audit trail that can stall the thing it audits is worse than one with a
  hole in it. 64 events is minutes of the busiest imaginable day, since links are
  edited by hand.
- **A webhook URL is a credential** — anyone with it can post to the channel. Two
  consequences: it belongs in the config file rather than on the command line, where
  the process list would show it; and **no error naming it may be logged**. The errors
  of `net/http` carry the URL they were given, so `postWebhook` unwraps `*url.Error`
  before returning. Verified: a failure logs
  `dial tcp 127.0.0.1:9: connect: connection refused` and the token appears nowhere.
- Slack reads `&`, `<` and `>` as markup, and a real pattern is full of ampersands, so
  every value is escaped. The link name is a Slack link to its detail page, built from
  the same hostname the UI uses; `linkHostname` is now shared with the `go` template
  function so the two cannot drift.
- Only `stopWebhook` exists for the tests' sake; the process otherwise runs until it is
  killed. `TestWebhook` drives real saves at an `httptest` server and checks both
  formats, that a refusing webhook does not fail the save, and that nothing is posted
  when no URL is set.
- **What is still not audited** is unchanged: a snapshot restore, and anything done to
  the database with `sqlite3`, including every change to `Admins`.

### 12. Exporting every link is an admin thing (**done**, commit `golink: let only admins export every link`)

`-admin-only-export`, off by default. Reading every link in one request is a different
act from following one, so it can be held to a different rule.

- **`/.export` returns 403** to anyone but an admin when the flag is set.
- **The pages stop offering the bulk URLs** to anyone who cannot use them: the
  "Download all links" footer on `/.all` and `/.search`, and the `/.export`,
  `/.export-stats` and `/.metrics` rows of the help page's Special links table, along
  with the walkthrough of `/.export` in its API section.
- **`/.export-stats` and `/.metrics` still answer anybody**, deliberately. Prometheus
  scrapes the pod directly and holds no session, so gating them would break the
  scrape rather than protect anything. Hiding them from the help page is about not
  advertising a diagnostic to people with no use for it; it is not a control, and the
  note in step 7 about never adding a `skip_auth_regex` still does the real work.
- `showBulkURLs` is the one predicate, used by `serveAll`, `serveSearch` and
  `serveHelp`. Those three read the user with `cu, _ := requestUser(r)`: a failure to
  say who somebody is means they are not an admin, which is the safe way round and
  leaves the page readable rather than turning it into a 500.

### 13. nginx, the local stack, and the scripts (**done**, commit `deploy: nginx, a local stack, and the scripts that run it`)

Three files in `deploy/nginx/` plus `compose.yaml`, none of which existed before, so
none of them can conflict on a rebase.

| File | What it is |
|---|---|
| `deploy/nginx/patterns.conf` | the `map` of bare patterns, shared by both server blocks |
| `deploy/nginx/golink.conf` | production: oauth2-proxy authenticates every request |
| `deploy/nginx/golink-dev.conf` | local: authenticates nobody, hands golink a fixed identity |
| `compose.yaml` | nginx + golink locally, using the dev config |

Mount `patterns.conf` as `00-patterns.conf` and the server block as `default.conf`.
The prefix orders the map ahead of the server, and taking over `default.conf` replaces
the nginx image's own default server, which would otherwise be the one nginx picks for
an unmatched Host.

**Verified against a running stack**, both configs, the production one with a stub
standing in for oauth2-proxy:

- `go/ABC-1234` and `go/ABC-1234/` redirect to Jira, deliberately *ahead* of the auth
  subrequest: it discloses nothing and keeps a Jira link working while a session
  expires. golink's own lowercase routes are untouched by the pattern.
- With no session, every other path returns the sign-in page. With a session, golink is
  served and a link created through nginx is owned by the session's user.
- **A request that forges `X-Auth-Request-Email` while holding a valid session still
  comes out as the session's user.** `proxy_set_header` redefines the header rather
  than adding to it, and an empty value drops it, so a client cannot smuggle one
  through. This is the property golink is trusting; if this config is ever rewritten,
  test this case again.
- Groups reach golink: the stub reported `infra@wetravel.com`, an `Admins` row of
  `group:infra@wetravel.com` was added, and that user could then edit a link locked by
  somebody else.

Two things learned the hard way while testing:

- **`proxy_pass` with a literal name resolves once, at startup.** Recreating the
  upstream container gave it a new address and nginx went on using the old one, 502ing
  until it was restarted. A Kubernetes Service has a stable ClusterIP so this is fine;
  it would not be fine pointed at a pod IP or a headless Service. Still no `resolver`:
  `127.0.0.11` is Docker's and absent under Podman.
- **The golink image is distroless: no shell, and no `sqlite3`.** So "manage admins with
  sqlite3" needs a plan. What works, and what the local stack was tested with, is an
  ephemeral container mounting the same volume:

  ```sh
  # locally, where the database is a bind mount:
  sqlite3 data/golink.db "INSERT INTO Admins (Name) VALUES ('you@wetravel.com');"

  # in the cluster: a one-off pod, or kubectl debug, mounting the PVC
  kubectl run golink-admin --rm -it --image=alpine --overrides='...' -- \
    sh -c 'apk add --no-cache sqlite && sqlite3 /data/golink.db'
  ```

  Quoting is easier from a file piped in than inline; two shells will happily turn
  `"group:x"` into a SQL identifier and fail with "no such column".

Also still true, and still worth not re-deriving: `proxy_set_header Host $http_host`,
not `$host`, which drops the port that golink builds its own URLs from. And **no
`skip_auth_regex`** for `/.` paths: `/.export` hands out every link in one request and
`/.metrics` names every link in its labels. Point Prometheus at the pod.

### 14. More than one instance (**done**, commit `golink: let more than one instance serve the same links`)

Three pieces of per-process state made a second replica misbehave in ways that a
health check would not have caught. All three are now shared or converged, so the
remaining obstacle to 2+ pods is the database, which is what step 15's MySQL work is
about; **two processes cannot share one SQLite file** — the second one fails at startup
with `database is locked (5)`, verified.

**The XSRF key.** `xsrfKey` was 32 random bytes drawn in `init()`, so each process
signed its forms with a different key and refused every form rendered by another: with
two pods behind a round-robin Service, roughly half of all saves would fail with
`invalid XSRF token`, and a retry would sometimes work, which is the worst way for a bug
to present. `-xsrf-key` (default `$GOLINK_XSRF_KEY`) sets it, and a key under 16
characters is refused at startup rather than accepted as a weak one. Unset, the
random per-process key remains, so a single instance needs no configuration.

- It is a **credential**: give it in the environment or `-config`, not on the command
  line where the process list would show it. Same reasoning as `-webhook-url` in step 11.
- Verified live with two processes: a form rendered by A and submitted to B returns
  **200** with a shared key and **400 invalid XSRF token** without one.
- `TestSharedXSRFKey` mints a token with one key and serves the request with another.
  The fixture owner is `foo@example.com`, the dev-mode user, so the token is the only
  thing that can decide the outcome.

**The click counter.** `stats.clicks` is an in-memory total flushed to the `Stats`
ledger every 5 seconds. Two instances each held their own total, so whichever flushed
last overwrote the other's — `INSERT OR REPLACE` on `(ID, Created)`, one row per minute.
`flushStats` now reads the totals back after saving, so each instance picks up what the
others recorded and the two converge within a flush. `TestFlushStatsReloadsTotals`
covers it.

**Shutdown.** Up to 5 seconds of clicks lived only in memory, and a rolling deploy kills
pods regularly. A SIGINT/SIGTERM handler flushes once and then re-raises the signal, so
the process still dies the way its supervisor expects and the exit status still says
"killed by signal". It is best-effort: a `SIGKILL` after the grace period, or a crash,
still loses whatever was not flushed. Clicks are a popularity heuristic, so that is
an acceptable trade; anything requiring exact counts would have to write through.

**Also shared, and already handled**: oauth2-proxy's session cookie. Its store is
stateless — the session is in the cookie — so no sticky sessions and no Redis are
needed, **provided every pod is given the same `--cookie-secret`**. A generated one, as
`start-prod.sh` does when the variable is unset, is single-instance only, and the script
says so on the way past. If Admin SDK group membership makes the cookie large, raise nginx's
`proxy_buffer_size`.

### 15. MySQL as a backend (**done**, commit `golink,db: store links in MySQL as well as SQLite`)

`-mysql user:password@tcp(host:3306)/golink` stores the links in MySQL instead of
in a SQLite file, which is what makes more than one replica possible at all: two
processes cannot share one SQLite file, and the second to start simply dies with
`database is locked (5)`. With MySQL, **verified with two instances against one
database**: a link created on A resolves on B, a form rendered by A is accepted by
B, and the clicks each recorded converge to the same total on both.

**One implementation, two dialects.** `DB` holds a `*sql.DB` and a `dialect`, and
every statement is written so that both databases read it the same way. Two sets of
queries would be twice the surface to keep right, and this fork has already broken
one of four copies of a query twice. What is actually dialect-specific is small:

| | why |
|---|---|
| `REPLACE INTO` | the long `INSERT OR REPLACE` is SQLite's spelling; the short one is in both |
| a REPLACE affects **2** rows in MySQL | it counts the delete and the insert; `dialect.replacedOneRow` allows either |
| `?` and never `?1` | MySQL has no numbered placeholders, so a repeated value is passed twice |
| `` `Long` `` in every statement naming it | **`LONG` is a reserved word in MySQL** and cannot be a bare identifier anywhere. SQLite reads a backtick as a quote for exactly this compatibility. This is why `SearchLinks` builds its query with `"` strings: a Go raw string cannot contain a backtick |
| `ESCAPE '!'` | MySQL reads a backslash inside a string literal before LIKE ever sees it. `containsPattern` escapes with `!` to match, and `Test_DB_SearchLinks` checks that `%`, `_` and `!` all mean themselves |
| the schema | separate files, since the types and the DDL differ |

**The schema.** `schema-mysql.sql` is `schema.sql` in MySQL's spelling and has to be
kept in step with it by hand. What differs, and why:

- `TEXT`/`INTEGER` become `VARCHAR(255)`/`BIGINT`, since a MySQL primary key needs a
  length. `Locked` is `TINYINT(1)`, which `database/sql` scans into a `bool`.
- `COLLATE NOCASE` on the Admins primary key becomes the table collation
  `utf8mb4_general_ci`, which is what makes the same name in two cases one row.
- **A MySQL `TEXT` column may not have a default**, so `Long` and `Pattern` have none
  and a row inserted by hand must give them. golink always does.
- `DEFAULT (UNIX_TIMESTAMP())` stands in for `strftime('%s','now')`, and needs
  **MySQL 8.0.13 or newer**. The `CHECK` on Admins needs 8.0.16. Both are only
  reached by a row inserted by hand, but a create would fail on an older server.
- The schema is applied **one statement at a time**: MySQL's driver refuses several
  in one `Exec` unless the DSN carries `multiStatements`, which is a setting that
  widens what any SQL injection could reach. `execSchema` splits on the semicolons
  after stripping the `--` comments, so a semicolon inside a comment is harmless --
  and there is one in each file, which is how that bug was found.

**Choosing one.** `-mysql` with `-sqlitedb` is a startup error rather than a silent
preference, since each names a database and a guess would be the wrong kind of
convenience. `-resolve-from-backup` ignores both, resolving against the snapshot file
in an in-memory database. **The DSN is a credential**: it defaults to
`$GOLINK_MYSQL_DSN`, belongs in the environment or `-config`, and no error mentions
it -- verified for an unreachable server, a malformed DSN and the both-flags case,
none of which printed the password.

**What is still per-process** is the `sync.RWMutex`, which is there for SQLite's
single writer. Two instances saving the same link at once is therefore last-write-
wins on the whole row, which is what it was between two browser tabs already; links
are edited by hand, seconds apart at worst.

**Connections** are capped at 8 with a 3-minute lifetime, so golink does not sit
holding one that a failover or a proxy has taken away underneath it. If the database
is unreachable at startup golink exits, which in the cluster is a crashloop with
backoff -- the right behaviour, since it will be restarted until MySQL answers.

**Testing against a real MySQL.** The storage tests run against SQLite by default, so
`go test ./...` needs no server. Point `$GOLINK_TEST_MYSQL_DSN` at a database and the
same tests run against that instead, which is the only thing that can prove the
statements above are read the same way by both:

```sh
podman run -d --name golink-mysql-test -e MYSQL_ROOT_PASSWORD=root \
    -e MYSQL_DATABASE=golink_test -p 3307:3306 docker.io/library/mysql:8.4
GOLINK_TEST_MYSQL_DSN='root:root@tcp(127.0.0.1:3307)/golink_test' go test ./...
```

They empty the database first, so give them one kept for testing. `newTestDB` is the
seam; the tests that go through the handlers still use SQLite in memory, because what
they are testing is not the storage.

**Locally**, `compose.mysql.yaml` is an override that swaps the backend of *either*
stack -- the golink service is defined identically in both -- and
`./start-dev.sh --mysql`, `./start-prod.sh --mysql` and `./import.sh --mysql` pass it.
The two backends hold **different links**; nothing carries them across but `/.export`
and `import.sh`. Its data is a named volume rather than a directory in the tree,
unlike the SQLite file, because a MySQL data directory is written from inside the
container and a bind mount makes that a permissions problem for nothing. It survives
a restart, a rebuild, a recreate and `podman compose down`; only `down -v`, a
`podman volume rm`, or a `podman system prune --volumes` throws it away -- which is one
notch less safe than the bind mount the SQLite file lives in, and the reason `/.export`
rather than the volume is the backup. Admins are managed with the `mysql` client instead
of `sqlite3`:

```sh
podman compose -f compose.yaml -f compose.mysql.yaml exec mysql \
    mysql -ugolink -pgolink golink -e "INSERT INTO Admins (Name) VALUES ('you@wetravel.com');"
```

`import.sh` now runs the importer as a one-off container of the golink **service**
(`podman compose run --build golink`), which is what puts it on the same network and
gives it the same database and environment as the stack. It used to run the image
directly with the volume named on the command line, which cannot reach a MySQL
container. The `--build` matters: without it the import runs whatever the image was
built from last, which was how "--sqlitedb is required" came out of a run whose
environment plainly had a DSN in it.

### 16. The cluster: two images, three apps, three repos

**Three apps make one service**, and they are three only because the deployment
chart gives an Application one image:

```
ALB (internal) -> wt-golink-nginx -> (subrequest) wt-oauth2-proxy -> Google
                                  -> wt-golink -> RDS MySQL
```

| | image | built from | why it is separate |
|---|---|---|---|
| `wt-golink` | `golink` | this repo's `Dockerfile` (upstream's, unchanged) | the app |
| `wt-golink-nginx` | `golink_nginx` | this repo's `deploy/nginx/Dockerfile` | the config is in the image, so one tag names one behaviour |
| `wt-oauth2-proxy` | `docker_base_images/oauth2-proxy` | nothing of ours -- mirror upstream's into ECR | nothing of ours in it |

**What goes where.** Three repos, and the split is not obvious from any of them:

- **this repo** -- the two Dockerfiles and the nginx config that is baked into one
  of them. Nothing else about the deployment.
- **`wetravel-com/argo-gitops`** -- the Kubernetes resources, as Helm *values* for
  the shared `helm/wt-service` chart. There are no manifests to write: one values
  file per app plus an element in the production ApplicationSet, and the chart
  renders the Deployment, Service, Ingress, PDB, VPA and NetworkPolicies.

  ```
  helm/values/wt_golink/{base,production}.yaml
  helm/values/wt_golink_nginx/{base,production}.yaml
  helm/values/wt_oauth2_proxy/{base,production}.yaml
  appsets/production.yaml            # one element per app: appName + valuesDir
  ```

  Values are layered `common/production.yaml` -> `<app>/base.yaml` ->
  `<app>/production.yaml`, last wins. Render before pushing:

  ```sh
  helm template test helm/wt-service -f helm/values/common/production.yaml \
      -f helm/values/wt_golink/base.yaml -f helm/values/wt_golink/production.yaml \
      --set release=production
  ```

- **`wetravel-com/infrastructure`** -- everything the values *reference* and cannot
  create: the ECR repositories (`aws/modules/ecr_registry`), the `golink` database
  and its `golink_prod` user on the production RDS, the Secret keys those values
  name, the DNS record for `go.wetravel.com`, and this repo's registration in
  `github/` so it gets the standard `build_and_test.yml` -- which is also what
  patches `imageTag` in argo-gitops after a build.

**Secrets the values name** (nothing here may reach a command line):

| ref | what |
|---|---|
| `@secret/mysql-users/host` | the RDS endpoint, as every other service reads it |
| `@secret/mysql-users/golink_prod` | that user's password; the DSN is assembled from the two in `GOLINK_MYSQL_DSN` |
| `@secret/golink/xsrfKey` | **the same in every pod**, or half the saves fail (step 14) |
| `@secret/golink/cookieSecret` | oauth2-proxy's, also the same in every pod |
| `@secret/golink/oauthClientId`, `oauthClientSecret` | the Google OAuth client |

**Everything listens on 9292.** Not a preference: the platform's NetworkPolicy
allows pod-to-pod traffic on ports 80 and 9292 and nothing else, so a golink on
8080 is reachable from nginx only by writing an extra policy. The chart's Service,
probe and Ingress all follow `port`, so one number settles it.

**Probes.** `-healthcheck-path=/healthcheck` is what makes the
chart's `startupProbe` and `readinessProbe` work, and it is *that* path because
the chart annotates every Service with a Datadog `http_check` pointing at
`/healthcheck` with no way to name another. nginx answers `/healthcheck` itself,
**before** the auth subrequest -- a probe that got a 302 to Google would never
report healthy, and an ALB target group with no healthy targets serves nothing.
oauth2-proxy is given `--ping-path=/healthcheck` for the same reason.

**Two things about nginx in the cluster:**

- It resolves `wt-golink` **once, at startup**, and refuses to start at all if the
  name does not exist (`host not found in upstream`). Deploying it before golink's
  Service therefore crashloops until that Service appears -- it recovers by itself,
  and sync waves do not order apps an ApplicationSet generates, so expect it on a
  first deploy. Resolve-once is otherwise right here: a Service keeps its ClusterIP
  for life, which is exactly what compose does not do (see local development).
- TLS ends at the ALB, so nginx sees plain HTTP. `forwarded.conf` takes the scheme
  from `X-Forwarded-Proto`; without it oauth2-proxy would send people back to an
  `http://` URL after signing in.

**The flags to get right**, in `command:` because the image's own CMD names a
SQLite file:

```
/golink -dev-listen=:9292 -open-links -admin-only-export
        -healthcheck-path=/healthcheck
        -auth-email-header=X-Auth-Request-Email -auth-groups-header=X-Auth-Request-Groups
```

`-dev-listen` is the flag for "serve plain HTTP, do not join a tailnet"; it is not
a debug mode. Forgetting `-open-links` silently gives you upstream's owner-locked
model. Forgetting `-auth-email-header` silently makes **everyone**
`foo@example.com`, the dev-mode user -- the one failure here that is silent rather
than closed, so assert on it after deploying: create a link and check its owner.
Add `-owner-can-lock` if locking should not be an admin-only decision. The image
runs as uid 65532 with home `/home/nonroot`. Back up with `/.export`, which is
also the only thing that moves links between backends.

**The open risk, stated plainly.** golink cannot tell an identity header nginx set
from one a client sent, and the chart's NetworkPolicy lets *any pod in the cluster*
reach a serving port ("authorization is the mesh's job"). So the guarantee this
design needs -- only nginx talks to golink -- is not what the platform's default
gives: any compromised pod could create, edit or delete links as anyone, and read
every link. `hasIngress: false` keeps golink off the internet, which is the larger
half. Closing the rest means one of:

- `networkPolicy.enabled: false` on the golink app plus hand-written policies
  through `extraNetworkPolicies` (a NetworkPolicy union cannot subtract, so the
  chart's permissive rule has to stop rendering rather than be narrowed);
- a Linkerd `AuthorizationPolicy`, which the chart does not render; or
- a shared secret header that nginx sets and golink requires -- the same shape as
  the `INTERNAL_API_KEY` every other service here uses. That is a change to this
  fork, roughly a flag and a check in `proxyUser`, and is not written.

**Verified locally against the k8s config**, with a stub in place of oauth2-proxy
and everything on 9292: `/healthcheck` answers 200 without a session, `go/ABC-1234`
redirects to Jira ahead of auth, a link created through nginx is owned by the
session's user, and **a request that forges `X-Auth-Request-Email` still comes out
as the session's user**. Re-run that last one if this config is ever rewritten.

**There is no schema migration, and that only bites after this is deployed.**
(And there are now two schema files to add the column to, not one.)
`schema.sql` is executed on every start, and `CREATE TABLE IF NOT EXISTS` creates a
missing *table* but never adds a column to a table that is already there. So a column
added once the database holds data will simply be absent, and every query naming it will
fail. Adding one then means putting an `ALTER TABLE` back into `newDB`, for whichever
dialects are in use, or running it by hand -- against RDS, which is a change no
rollback of the image undoes, so the column has to be added before the code that
needs it ships. There was such a migration during development, for
databases that only ever existed on one laptop; the shape to copy is in

```sh
git show backup_initial_set_of_changes:db.go   # the last version with migrate() in it
```

**What is left is not migration**, though it reads like it. `legacyPattern` has two
callers:

- `Link.UnmarshalJSON`, so that a snapshot written before `Pattern` existed restores to
  links that answer the same URLs. This guards backup **files**, which outlive any
  database and can turn up from anywhere, so it is worth keeping until every snapshot
  in existence is known to be new-format.
- `serveSave`, where a template arriving in `long` with no `pattern` is moved across.
  That is an API convenience for clients written before the field, not a migration, and
  the help page still documents `long` on its own.

---

## Local development

```bash
./start-dev.sh          # http://localhost:8080/, loopback only
./start-dev.sh 80       # http://localhost/
./start-dev.sh --mysql  # the same, with the links in MySQL rather than SQLite
open http://localhost:8080/fadsfads   # should render the create form, name pre-filled
```

`--mysql` works on `start-prod.sh` and `import.sh` too, and is a **different set of
links**: see step 15.

`start-prod.sh` runs the same three parts as the cluster -- nginx, a real
oauth2-proxy against Google, golink -- on this machine, from `compose.prod.yaml`.
It needs a Google OAuth client whose authorised redirect URIs include
`http://localhost:8080/oauth2/callback`, and it is the way to settle which header
oauth2-proxy actually sets before trusting the guess in the flags.

**podman will bind a privileged port only to every address, never to one.** So
`./start-dev.sh 80` publishes on all interfaces and says so loudly, since the dev
stack authenticates nobody; anything from 1024 up is bound to loopback. The
alternative is a `pfctl` redirect, which the script prints.

That stack is nginx in front of golink, shaped like the deployment but authenticating
nobody: nginx tells golink every visitor is `dev@wetravel.com` in `infra@wetravel.com`.
Edit those two headers in `deploy/nginx/golink-dev.conf` and `podman compose restart
nginx` to be somebody else. `podman compose down -v` throws the links away.

**After a rebuild, restart nginx**: `podman compose up -d --build` recreates the golink
container with a new address, and nginx resolved the old one at startup, so it will 502
until `podman compose restart nginx`. Same root cause as the note in step 7; both start
scripts do it for you, which is most of why they exist.

**`podman compose restart` is not how to bring the stack back** -- `./start-dev.sh` is.
Restart starts every container at once, and two things then go wrong at once: golink
pings MySQL, finds it not yet listening, and exits, whereupon nginx will not start
either, because it refuses to resolve an upstream that is not there
(`host not found in upstream "golink"`). Both were observed, and both are why every
service now carries `restart: unless-stopped` -- with it, nothing stays dead. What the
policies cannot fix is nginx still holding golink's *old* address, so a restarted stack
serves 502 until nginx is restarted after golink, which is exactly what the start
scripts do last. **None of this touches the links**: verified by killing every container
in the MySQL stack and bringing it back with all 47 still there.

**The database is `./data/golink.db`, a bind mount, not a named volume.** A named
volume was silently removed once by a `podman compose` invocation meant only to swap
between the dev and the production stack, taking the links with it, and the loss was
not reproducible afterwards. A directory in the working tree cannot be removed by
anything compose does -- verified against `podman compose down -v` -- and it also means
`sqlite3 data/golink.db` works from the host, which is how admins are added locally.
`./import.sh <export.jsonl>` restores an export into it, and
`./import.sh --mysql <export.jsonl>` into the MySQL container instead.

**Mounting a file from `/tmp` fails on macOS.** The podman VM has `/private/tmp` and
`/Users`, and `/tmp` is only a symlink to the first of those, so `-v /tmp/x:/x` gives
`statfs: no such file or directory`. Use the real path.

The audit log is on golink's stdout, so `podman compose logs golink` is the local
version of what Datadog would be shipping.

golink's own port is not published, because production requires that nothing but nginx
can reach it. To test the flags without any of this, run the binary directly:

```bash
go run ./cmd/golink -dev-listen 127.0.0.1:8080 -open-links -sqlitedb "$PWD/golink.db"
```

Test after every resolution change: `TestServeGo` covers names with slashes, which
name wins, and what a path below a link with no pattern does; `TestServeSavePattern`
covers keeping the two fields apart; `TestRestoreSnapshot` covers reading a snapshot
from each of the three eras.

Test after every permission change. `TestCanEditLink`, `TestServeSave`,
`TestServeSaveLock` and `TestServeDelete` cover this matrix in both modes; keep them
that way, because the upstream-mode rows are the evidence that the fork is a strict
addition:

with `-open-links`:

- unlocked link edited by a non-owner → allowed, and the owner does not change
- locked link edited by a non-owner → 403
- locked link edited by an admin → allowed
- locked link edited by its owner → allowed
- lock toggled by anyone but an admin → 403, including by the owner and including
  on a link being created; with `-owner-can-lock`, the owner is allowed and others
  still are not
- update that omits `lockedset` → lock unchanged

without it (upstream behaviour):

- any link edited by a non-owner → 403
- no lock checkbox on the detail page

either way:

- `-readonly` → all edits refused

## Conventions

- `git commit -s` (DCO) — keeps the option of upstreaming open.
- Keep changes inside `canEditLink`, `requestUser`, the `Link` struct, `schema.sql`
  and `tmpl/detail.html`. Leave the `currentUser` var itself alone until step 3.
  The smaller and more localised the diff, the cheaper every rebase.
- Rebase on upstream `main` periodically; it's mostly dependency bumps.