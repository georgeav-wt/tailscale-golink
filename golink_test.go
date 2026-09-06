// Copyright 2022 Tailscale Inc & Contributors
// SPDX-License-Identifier: BSD-3-Clause

package golink

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"golang.org/x/net/xsrftoken"
	"tailscale.com/client/tailscale/apitype"
	"tailscale.com/tailcfg"
	"tailscale.com/tstest"
	"tailscale.com/types/ptr"
	"tailscale.com/util/must"
)

func init() {
	// tests always need golink to be run in dev mode
	*dev = ":8080"
	// and they should not print an audit log; TestAuditLog reads its own.
	auditWriter = io.Discard
}

func TestServeGo(t *testing.T) {
	var err error
	db, err = NewSQLiteDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.Save(&Link{Short: "who", Long: "http://who/", Pattern: "http://who/{{.Path}}"})
	db.Save(&Link{Short: "me", Pattern: "/who/{{.User}}"})
	db.Save(&Link{Short: "invalid-var", Pattern: "/who/{{.Invalid}}"})
	db.Save(&Link{Short: "team", Long: "http://team/"})
	db.Save(&Link{Short: "team/jira", Long: "http://jira/team"})
	db.Save(&Link{Short: "team/board", Pattern: "http://board/{{.Path}}"})

	tests := []struct {
		name        string
		link        string
		currentUser func(*http.Request) (user, error)
		wantStatus  int
		wantLink    string
	}{
		{
			name:       "simple link",
			link:       "/who",
			wantStatus: http.StatusFound,
			wantLink:   "http://who/",
		},
		{
			name:        "simple link, anonymous request",
			link:        "/who",
			currentUser: func(*http.Request) (user, error) { return user{}, nil },
			wantStatus:  http.StatusFound,
			wantLink:    "http://who/",
		},
		{
			name:       "simple link with path",
			link:       "/who/p",
			wantStatus: http.StatusFound,
			wantLink:   "http://who/p",
		},
		{
			name:       "simple link with query",
			link:       "/who?q=1",
			wantStatus: http.StatusFound,
			wantLink:   "http://who/?q=1",
		},
		{
			name:       "simple link with path and query",
			link:       "/who/p?q=1",
			wantStatus: http.StatusFound,
			wantLink:   "http://who/p?q=1",
		},
		{
			name:       "simple link with double slash in path",
			link:       "/who/http://host",
			wantStatus: http.StatusFound,
			wantLink:   "http://who/http://host",
		},
		{
			name:       "simple link, trailing period",
			link:       "/who.",
			wantStatus: http.StatusFound,
			wantLink:   "http://who/",
		},
		{
			name:       "simple link, trailing comma",
			link:       "/who,",
			wantStatus: http.StatusFound,
			wantLink:   "http://who/",
		},
		{
			// This seems like an incredibly unlikely typo, but test it anyway.
			name:       "simple link, trailing comma and path",
			link:       "/who,/p",
			wantStatus: http.StatusFound,
			wantLink:   "http://who/p",
		},
		{
			name:       "simple link, trailing paren",
			link:       "/who)",
			wantStatus: http.StatusFound,
			wantLink:   "http://who/",
		},
		{
			name:       "user link",
			link:       "/me",
			wantStatus: http.StatusFound,
			wantLink:   "/who/foo@example.com",
		},
		{
			name:       "link with a slash in its name",
			link:       "/team/jira",
			wantStatus: http.StatusFound,
			wantLink:   "http://jira/team",
		},
		{
			name:       "link with a slash in its name, trailing period",
			link:       "/team/jira.",
			wantStatus: http.StatusFound,
			wantLink:   "http://jira/team",
		},
		{
			// "team" is not dynamic, so it answers to its own name only and a
			// path below it is free to be a link of its own.
			name:       "path below a link that is not dynamic",
			link:       "/team/nothing-here",
			wantStatus: http.StatusNotFound,
		},
		{
			name:       "path below a link that is not dynamic, deeper",
			link:       "/team/jira/sub",
			wantStatus: http.StatusNotFound,
		},
		{
			// The longest name that is dynamic wins, even with a shorter
			// dynamic name above it.
			name:       "path below a dynamic link with a slash in its name",
			link:       "/team/board/42",
			wantStatus: http.StatusFound,
			wantLink:   "http://board/42",
		},
		{
			name:       "unknown link",
			link:       "/does-not-exist",
			wantStatus: http.StatusNotFound,
		},
		{
			name:       "unknown link with a slash",
			link:       "/does-not/exist",
			wantStatus: http.StatusNotFound,
		},
		{
			name:       "unknown variable",
			link:       "/invalid-var",
			wantStatus: http.StatusInternalServerError,
		},
		{
			name:        "user link, anonymous request",
			link:        "/me",
			currentUser: func(*http.Request) (user, error) { return user{}, nil },
			wantStatus:  http.StatusUnauthorized,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.currentUser != nil {
				oldCurrentUser := currentUser
				currentUser = tt.currentUser
				t.Cleanup(func() {
					currentUser = oldCurrentUser
				})
			}

			r := httptest.NewRequest("GET", tt.link, nil)
			w := httptest.NewRecorder()
			serveHandler().ServeHTTP(w, r)

			if w.Code != tt.wantStatus {
				t.Errorf("serveGo(%q) = %d; want %d", tt.link, w.Code, tt.wantStatus)
			}
			if gotLink := w.Header().Get("Location"); gotLink != tt.wantLink {
				t.Errorf("serveGo(%q) = %q; want %q", tt.link, gotLink, tt.wantLink)
			}
		})
	}
}

func TestReferrerPolicy(t *testing.T) {
	var err error
	db, err = NewSQLiteDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.Save(&Link{Short: "who", Long: "http://who/"})

	// all responses should ask the browser to suppress the Referer header,
	// so that link destinations never learn the golink host.
	for _, path := range []string{"/", "/who", "/.detail/who"} {
		r := httptest.NewRequest("GET", path, nil)
		w := httptest.NewRecorder()
		serveHandler().ServeHTTP(w, r)

		if got := w.Header().Get("Referrer-Policy"); got != "no-referrer" {
			t.Errorf("GET %q: Referrer-Policy = %q; want %q", path, got, "no-referrer")
		}
	}
}

func TestServeSave(t *testing.T) {
	var err error
	db, err = NewSQLiteDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.Save(&Link{Short: "link-owned-by-tagged-devices", Long: "/before", Owner: "tagged-devices"})
	db.Save(&Link{Short: "locked-link", Long: "/before", Owner: "foo@example.com", Locked: true})
	db.Save(&Link{Short: "lockme", Long: "/before", Owner: "foo@example.com"})

	fooXSRF := func(short string) string {
		return xsrftoken.Generate(xsrfKey, "foo@example.com", short)
	}
	barXSRF := func(short string) string {
		return xsrftoken.Generate(xsrfKey, "bar@example.com", short)
	}

	tests := []struct {
		name              string
		short             string
		xsrf              string
		long              string
		lockedset         bool // whether to submit the lock state at all
		locked            bool // the lock state to submit
		openLinks         bool
		ownerCanLock      bool
		allowUnknownUsers bool
		currentUser       func(*http.Request) (user, error)
		wantStatus        int
		wantLocation      string
	}{
		{
			name:       "missing short",
			short:      "",
			long:       "http://who/",
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "missing long",
			short:      "",
			long:       "http://who/",
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "save simple link",
			short:      "who",
			xsrf:       fooXSRF(newShortName),
			long:       "http://who/",
			wantStatus: http.StatusOK,
		},
		{
			name:        "disallow editing another's link",
			short:       "who",
			xsrf:        barXSRF("who"),
			long:        "http://who/",
			currentUser: func(*http.Request) (user, error) { return user{login: "bar@example.com"}, nil },
			wantStatus:  http.StatusForbidden,
		},
		{
			name:        "open links: allow editing another's unlocked link",
			short:       "who",
			xsrf:        barXSRF("who"),
			long:        "http://who/",
			openLinks:   true,
			currentUser: func(*http.Request) (user, error) { return user{login: "bar@example.com"}, nil },
			wantStatus:  http.StatusOK,
		},
		{
			name:        "open links: disallow editing another's locked link",
			short:       "locked-link",
			xsrf:        barXSRF("locked-link"),
			long:        "/after",
			openLinks:   true,
			currentUser: func(*http.Request) (user, error) { return user{login: "bar@example.com"}, nil },
			wantStatus:  http.StatusForbidden,
		},
		{
			name:       "open links: allow owner to edit their own locked link",
			short:      "locked-link",
			xsrf:       fooXSRF("locked-link"),
			long:       "/after",
			openLinks:  true,
			wantStatus: http.StatusOK,
		},
		{
			name:        "open links: allow admin to edit another's locked link",
			short:       "locked-link",
			xsrf:        barXSRF("locked-link"),
			long:        "/after",
			openLinks:   true,
			currentUser: func(*http.Request) (user, error) { return user{login: "bar@example.com", isAdmin: true}, nil },
			wantStatus:  http.StatusOK,
		},
		{
			// Locking is an admin decision, so its owner cannot.
			name:       "open links: disallow the owner from locking a link",
			short:      "lockme",
			xsrf:       fooXSRF("lockme"),
			long:       "/after",
			openLinks:  true,
			lockedset:  true,
			locked:     true,
			wantStatus: http.StatusForbidden,
		},
		{
			name:         "owner can lock: allow the owner to lock a link",
			short:        "lockme",
			xsrf:         fooXSRF("lockme"),
			long:         "/after",
			openLinks:    true,
			ownerCanLock: true,
			lockedset:    true,
			locked:       true,
			wantStatus:   http.StatusOK,
		},
		{
			name:        "open links: allow an admin to unlock a link",
			short:       "lockme",
			xsrf:        barXSRF("lockme"),
			long:        "/after",
			openLinks:   true,
			lockedset:   true,
			currentUser: func(*http.Request) (user, error) { return user{login: "bar@example.com", isAdmin: true}, nil },
			wantStatus:  http.StatusOK,
		},
		{
			// A new link cannot be born locked either, or the rule would only
			// hold for links that already exist.
			name:       "open links: disallow creating a link already locked",
			short:      "born-locked",
			xsrf:       fooXSRF(newShortName),
			long:       "/x",
			openLinks:  true,
			lockedset:  true,
			locked:     true,
			wantStatus: http.StatusForbidden,
		},
		{
			name:        "open links: disallow non-owner from locking a link",
			short:       "who",
			xsrf:        barXSRF("who"),
			long:        "http://who/",
			openLinks:   true,
			lockedset:   true,
			locked:      true,
			currentUser: func(*http.Request) (user, error) { return user{login: "bar@example.com"}, nil },
			wantStatus:  http.StatusForbidden,
		},
		{
			name:        "allow editing link owned by tagged-devices",
			short:       "link-owned-by-tagged-devices",
			xsrf:        barXSRF("link-owned-by-tagged-devices"),
			long:        "/after",
			currentUser: func(*http.Request) (user, error) { return user{login: "bar@example.com"}, nil },
			wantStatus:  http.StatusOK,
		},
		{
			name:        "admins can edit any link",
			short:       "who",
			xsrf:        barXSRF("who"),
			long:        "http://who/",
			currentUser: func(*http.Request) (user, error) { return user{login: "bar@example.com", isAdmin: true}, nil },
			wantStatus:  http.StatusOK,
		},
		{
			name:        "disallow unknown users",
			short:       "who2",
			xsrf:        fooXSRF("who2"),
			long:        "http://who/",
			currentUser: func(*http.Request) (user, error) { return user{}, errors.New("") },
			wantStatus:  http.StatusInternalServerError,
		},
		{
			name:              "allow unknown users",
			short:             "who2",
			long:              "http://who/",
			allowUnknownUsers: true,
			currentUser:       func(*http.Request) (user, error) { return user{}, nil },
			wantStatus:        http.StatusOK,
		},
		{
			name:         "redirect to detail page when creating link that already exists",
			short:        "who",
			xsrf:         barXSRF(newShortName),
			long:         "http://who/updated",
			currentUser:  func(*http.Request) (user, error) { return user{login: "bar@example.com", isAdmin: true}, nil },
			wantStatus:   http.StatusSeeOther,
			wantLocation: "/.detail/who?exists=1",
		},
		{
			name:       "save link with a slash in its name",
			short:      "team/jira",
			xsrf:       fooXSRF(newShortName),
			long:       "http://jira/team",
			wantStatus: http.StatusOK,
		},
		{
			name:         "redirect to detail page for a name with a slash",
			short:        "team/jira",
			xsrf:         fooXSRF(newShortName),
			long:         "http://jira/team/updated",
			wantStatus:   http.StatusSeeOther,
			wantLocation: "/.detail/team/jira?exists=1",
		},
		{
			name:       "leading slash",
			short:      "/team",
			xsrf:       fooXSRF(newShortName),
			long:       "http://team/",
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "trailing slash",
			short:      "team/",
			xsrf:       fooXSRF(newShortName),
			long:       "http://team/",
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "empty segment",
			short:      "team//jira",
			xsrf:       fooXSRF(newShortName),
			long:       "http://team/",
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "segment starting with a dot",
			short:      "team/.export",
			xsrf:       fooXSRF(newShortName),
			long:       "http://team/",
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "invalid xsrf",
			short:      "goat",
			xsrf:       fooXSRF("sheep"),
			long:       "https://goat.example.com/goat.php?goat=true",
			wantStatus: http.StatusBadRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.currentUser != nil {
				oldCurrentUser := currentUser
				currentUser = tt.currentUser
				t.Cleanup(func() {
					currentUser = oldCurrentUser
				})
			}

			oldAllowUnknownUsers := *allowUnknownUsers
			*allowUnknownUsers = tt.allowUnknownUsers
			t.Cleanup(func() { *allowUnknownUsers = oldAllowUnknownUsers })

			oldOpenLinks, oldOwnerCanLock := *openLinks, *ownerCanLock
			*openLinks, *ownerCanLock = tt.openLinks, tt.ownerCanLock
			t.Cleanup(func() { *openLinks, *ownerCanLock = oldOpenLinks, oldOwnerCanLock })

			form := url.Values{
				"short": {tt.short},
				"long":  {tt.long},
				"xsrf":  {tt.xsrf},
			}
			if tt.lockedset {
				form.Set("lockedset", "1")
				if tt.locked {
					form.Set("locked", "1")
				}
			}
			r := httptest.NewRequest("POST", "/", strings.NewReader(form.Encode()))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			w := httptest.NewRecorder()
			serveSave(w, r)

			if w.Code != tt.wantStatus {
				t.Errorf("serveSave(%q, %q) = %d; want %d", tt.short, tt.long, w.Code, tt.wantStatus)
			}
			if tt.wantLocation != "" {
				if got := w.Header().Get("Location"); got != tt.wantLocation {
					t.Errorf("serveSave(%q, %q) Location = %q; want %q", tt.short, tt.long, got, tt.wantLocation)
				}
			}
		})
	}
}

// TestServeSaveLock tests that a link's lock is only changed when a request
// explicitly says so, and that editing a link does not take ownership of it.
// Who is allowed to change a lock is TestCanLockLink's business; this runs with
// -owner-can-lock so that the owner can drive it.
func TestServeSaveLock(t *testing.T) {
	oldOpenLinks, oldOwnerCanLock := *openLinks, *ownerCanLock
	*openLinks, *ownerCanLock = true, true
	t.Cleanup(func() { *openLinks, *ownerCanLock = oldOpenLinks, oldOwnerCanLock })

	var err error
	db, err = NewSQLiteDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.Save(&Link{Short: "lk", Long: "/before", Owner: "foo@example.com"})

	save := func(t *testing.T, form url.Values, cu user) {
		t.Helper()
		oldCurrentUser := currentUser
		currentUser = func(*http.Request) (user, error) { return cu, nil }
		t.Cleanup(func() { currentUser = oldCurrentUser })

		form.Set("xsrf", xsrftoken.Generate(xsrfKey, cu.login, form.Get("short")))
		r := httptest.NewRequest("POST", "/", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		serveSave(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("serveSave(%v) = %d (%s); want 200", form, w.Code, w.Body.String())
		}
	}
	load := func(t *testing.T) *Link {
		t.Helper()
		link, err := db.Load("lk")
		if err != nil {
			t.Fatal(err)
		}
		return link
	}

	foo := user{login: "foo@example.com"}
	bar := user{login: "bar@example.com"}

	// The owner locks the link.
	save(t, url.Values{"short": {"lk"}, "long": {"/after"}, "lockedset": {"1"}, "locked": {"1"}}, foo)
	if link := load(t); !link.Locked {
		t.Error("link not locked after saving with locked=1")
	}

	// A request that says nothing about the lock leaves it alone.
	save(t, url.Values{"short": {"lk"}, "long": {"/after2"}}, foo)
	if link := load(t); !link.Locked {
		t.Error("link unlocked by a request that did not set lockedset")
	}

	// The owner unlocks the link. An unchecked checkbox submits no value at
	// all, so only lockedset is present.
	save(t, url.Values{"short": {"lk"}, "long": {"/after3"}, "lockedset": {"1"}}, foo)
	if link := load(t); link.Locked {
		t.Error("link still locked after saving with lockedset and no locked")
	}

	// Another user may now edit the link, but does not become its owner.
	save(t, url.Values{"short": {"lk"}, "long": {"/after4"}}, bar)
	if link := load(t); link.Owner != "foo@example.com" {
		t.Errorf("link owner = %q after edit by another user; want foo@example.com", link.Owner)
	}
}

// TestAdminsTable tests that a login listed in the Admins table is treated as
// an admin, which otherwise only a tailnet ACL grant can confer. It runs
// without -open-links, where admin rights are the only way to edit a link
// belonging to somebody else.
func TestAdminsTable(t *testing.T) {
	var err error
	db, err = NewSQLiteDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.Save(&Link{Short: "lk", Long: "/before", Owner: "foo@example.com"})
	if _, err := db.db.Exec(`INSERT INTO Admins (Name) VALUES ('admin@example.com')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`INSERT INTO Admins (Name) VALUES ('group:admins@example.com')`); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name       string
		login      string
		groups     []string
		wantStatus int
	}{
		{name: "login absent from the table", login: "bar@example.com", wantStatus: http.StatusForbidden},
		{name: "login present in the table", login: "admin@example.com", wantStatus: http.StatusOK},
		{name: "group absent from the table", login: "bar@example.com", groups: []string{"sales@example.com"}, wantStatus: http.StatusForbidden},
		{name: "group present in the table", login: "bar@example.com", groups: []string{"admins@example.com"}, wantStatus: http.StatusOK},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oldCurrentUser := currentUser
			currentUser = func(*http.Request) (user, error) {
				return user{login: tt.login, groups: tt.groups}, nil
			}
			t.Cleanup(func() { currentUser = oldCurrentUser })

			form := url.Values{
				"short": {"lk"},
				"long":  {"/after"},
				"owner": {"foo@example.com"},
				"xsrf":  {xsrftoken.Generate(xsrfKey, tt.login, "lk")},
			}
			r := httptest.NewRequest("POST", "/", strings.NewReader(form.Encode()))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			w := httptest.NewRecorder()
			serveSave(w, r)

			if w.Code != tt.wantStatus {
				t.Errorf("serveSave as %q in %q = %d (%s); want %d", tt.login, tt.groups, w.Code, w.Body.String(), tt.wantStatus)
			}
		})
	}
}

func TestProxyUser(t *testing.T) {
	tests := []struct {
		name              string
		emailHeader       string
		groupsHeader      string
		headers           map[string][]string
		allowUnknownUsers bool
		want              user
		wantErr           bool
	}{
		{
			name:        "email header",
			emailHeader: "X-Auth-Request-Email",
			headers:     map[string][]string{"X-Auth-Request-Email": {"amelie@example.com"}},
			want:        user{login: "amelie@example.com"},
		},
		{
			name:         "email and groups headers",
			emailHeader:  "X-Auth-Request-Email",
			groupsHeader: "X-Auth-Request-Groups",
			headers: map[string][]string{
				"X-Auth-Request-Email":  {"amelie@example.com"},
				"X-Auth-Request-Groups": {"eng@example.com,admins@example.com"},
			},
			want: user{login: "amelie@example.com", groups: []string{"eng@example.com", "admins@example.com"}},
		},
		{
			name:         "groups header with spaces and empty entries",
			emailHeader:  "X-Auth-Request-Email",
			groupsHeader: "X-Auth-Request-Groups",
			headers: map[string][]string{
				"X-Auth-Request-Email":  {" amelie@example.com "},
				"X-Auth-Request-Groups": {" eng@example.com , , admins@example.com "},
			},
			want: user{login: "amelie@example.com", groups: []string{"eng@example.com", "admins@example.com"}},
		},
		{
			name:         "groups header not sent",
			emailHeader:  "X-Auth-Request-Email",
			groupsHeader: "X-Auth-Request-Groups",
			headers:      map[string][]string{"X-Auth-Request-Email": {"amelie@example.com"}},
			want:         user{login: "amelie@example.com"},
		},
		{
			name:        "groups header sent but not configured",
			emailHeader: "X-Auth-Request-Email",
			headers:     map[string][]string{"X-Auth-Request-Email": {"amelie@example.com"}, "X-Auth-Request-Groups": {"eng@example.com"}},
			want:        user{login: "amelie@example.com"},
		},
		{
			name:         "groups header repeated instead of comma-separated",
			emailHeader:  "X-Auth-Request-Email",
			groupsHeader: "X-Auth-Request-Groups",
			headers: map[string][]string{
				"X-Auth-Request-Email":  {"amelie@example.com"},
				"X-Auth-Request-Groups": {"eng@example.com", "admins@example.com"},
			},
			want: user{login: "amelie@example.com", groups: []string{"eng@example.com", "admins@example.com"}},
		},
		{
			name:        "email header missing",
			emailHeader: "X-Auth-Request-Email",
			headers:     map[string][]string{"X-Auth-Request-Groups": {"eng@example.com"}},
			wantErr:     true,
		},
		{
			name:              "email header missing, unknown users allowed",
			emailHeader:       "X-Auth-Request-Email",
			headers:           map[string][]string{},
			allowUnknownUsers: true,
			want:              user{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oldEmailHeader, oldGroupsHeader, oldAllowUnknownUsers := *authEmailHeader, *authGroupsHeader, *allowUnknownUsers
			*authEmailHeader, *authGroupsHeader, *allowUnknownUsers = tt.emailHeader, tt.groupsHeader, tt.allowUnknownUsers
			t.Cleanup(func() {
				*authEmailHeader, *authGroupsHeader, *allowUnknownUsers = oldEmailHeader, oldGroupsHeader, oldAllowUnknownUsers
			})

			r := httptest.NewRequest("GET", "/", nil)
			for k, values := range tt.headers {
				for _, v := range values {
					r.Header.Add(k, v)
				}
			}

			got, err := proxyUser(r)
			if (err != nil) != tt.wantErr {
				t.Fatalf("proxyUser() error = %v; wantErr %v", err, tt.wantErr)
			}
			if err != nil {
				return
			}
			if !cmp.Equal(got, tt.want, cmp.AllowUnexported(user{})) {
				t.Errorf("proxyUser() = %+v; want %+v", got, tt.want)
			}
		})
	}
}

// TestProxyUserAdmin tests the two halves of running behind an authenticating
// proxy together: the groups the proxy reports make a user an admin when the
// Admins table lists one of them.
func TestProxyUserAdmin(t *testing.T) {
	oldEmailHeader, oldGroupsHeader, oldOpenLinks := *authEmailHeader, *authGroupsHeader, *openLinks
	*authEmailHeader, *authGroupsHeader, *openLinks = "X-Auth-Request-Email", "X-Auth-Request-Groups", true
	t.Cleanup(func() {
		*authEmailHeader, *authGroupsHeader, *openLinks = oldEmailHeader, oldGroupsHeader, oldOpenLinks
	})

	oldCurrentUser := currentUser
	currentUser = proxyUser
	t.Cleanup(func() { currentUser = oldCurrentUser })

	var err error
	db, err = NewSQLiteDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.Save(&Link{Short: "lk", Long: "/before", Owner: "amelie@example.com", Locked: true})
	if _, err := db.db.Exec(`INSERT INTO Admins (Name) VALUES ('group:admins@example.com')`); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name       string
		groups     string
		wantStatus int
	}{
		{name: "no groups", groups: "", wantStatus: http.StatusForbidden},
		{name: "groups without the admin group", groups: "eng@example.com", wantStatus: http.StatusForbidden},
		{name: "groups including the admin group", groups: "eng@example.com,admins@example.com", wantStatus: http.StatusOK},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			form := url.Values{
				"short": {"lk"},
				"long":  {"/after"},
				"owner": {"amelie@example.com"},
				"xsrf":  {xsrftoken.Generate(xsrfKey, "bob@example.com", "lk")},
			}
			r := httptest.NewRequest("POST", "/", strings.NewReader(form.Encode()))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			r.Header.Set("X-Auth-Request-Email", "bob@example.com")
			r.Header.Set("X-Auth-Request-Groups", tt.groups)
			w := httptest.NewRecorder()
			serveSave(w, r)

			if w.Code != tt.wantStatus {
				t.Errorf("serveSave in groups %q = %d (%s); want %d", tt.groups, w.Code, w.Body.String(), tt.wantStatus)
			}
		})
	}
}

// TestServeSavePattern tests that a save keeps a link's destination and its
// pattern apart, and that a template arriving in the destination, which is how
// a link used to say it answered for the paths below it, is understood.
func TestServeSavePattern(t *testing.T) {
	var err error
	db, err = NewSQLiteDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name        string
		short       string
		form        url.Values
		wantStatus  int
		wantLong    string
		wantPattern string
	}{
		{
			name:       "destination only",
			short:      "docs",
			form:       url.Values{"long": {"https://wiki/docs"}},
			wantStatus: http.StatusOK,
			wantLong:   "https://wiki/docs",
		},
		{
			name:        "destination and pattern",
			short:       "code",
			form:        url.Values{"long": {"https://github.com/org"}, "pattern": {"https://github.com/search?q={{QueryEscape .Path}}"}},
			wantStatus:  http.StatusOK,
			wantLong:    "https://github.com/org",
			wantPattern: "https://github.com/search?q={{QueryEscape .Path}}",
		},
		{
			name:        "pattern only",
			short:       "jira",
			form:        url.Values{"pattern": {"https://jira/browse/{{.Path}}"}},
			wantStatus:  http.StatusOK,
			wantPattern: "https://jira/browse/{{.Path}}",
		},
		{
			// What a client that predates the pattern field sends.
			name:        "template in the destination",
			short:       "legacy",
			form:        url.Values{"long": {"https://jira/browse/{{.Path}}"}},
			wantStatus:  http.StatusOK,
			wantPattern: "https://jira/browse/{{.Path}}",
		},
		{
			// What the forms send: a marker saying the checkbox is present.
			name:        "dynamic ticked",
			short:       "ticked",
			form:        url.Values{"long": {"https://a/"}, "dynamicset": {"1"}, "dynamic": {"1"}, "pattern": {"https://a/{{.Path}}"}},
			wantStatus:  http.StatusOK,
			wantLong:    "https://a/",
			wantPattern: "https://a/{{.Path}}",
		},
		{
			// The field is hidden rather than disabled, so it arrives even
			// when the checkbox says the link is not dynamic.
			name:       "dynamic unticked with a pattern still in the form",
			short:      "unticked",
			form:       url.Values{"long": {"https://a/"}, "dynamicset": {"1"}, "pattern": {"https://a/{{.Path}}"}},
			wantStatus: http.StatusOK,
			wantLong:   "https://a/",
		},
		{
			// A template cannot be a destination, and the form said this link
			// has no pattern, so there is nowhere for it to go.
			name:       "dynamic unticked with a template in the destination",
			short:      "unticked2",
			form:       url.Values{"long": {"https://a/{{.Path}}"}, "dynamicset": {"1"}},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "template in the destination alongside a pattern",
			short:      "both",
			form:       url.Values{"long": {"https://a/{{.Path}}"}, "pattern": {"https://b/{{.Path}}"}},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "a destination with no scheme gains one",
			short:      "scheme",
			form:       url.Values{"long": {"g.co/test"}},
			wantStatus: http.StatusOK,
			wantLong:   "https://g.co/test",
		},
		{
			name:        "a pattern with no scheme gains one",
			short:       "scheme2",
			form:        url.Values{"pattern": {"g.co/{{.Path}}"}},
			wantStatus:  http.StatusOK,
			wantPattern: "https://g.co/{{.Path}}",
		},
		{
			name:       "an alias of another link keeps its leading slash",
			short:      "alias",
			form:       url.Values{"long": {"/scheme"}},
			wantStatus: http.StatusOK,
			wantLong:   "/scheme",
		},
		{
			name:       "neither destination nor pattern",
			short:      "empty",
			form:       url.Values{},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "invalid template in pattern",
			short:      "broken",
			form:       url.Values{"pattern": {"https://a/{{.Path"}},
			wantStatus: http.StatusBadRequest,
		},
		{
			// The pattern is dropped when the form says so, which is how a
			// link stops answering for the paths below it.
			name:       "pattern cleared",
			short:      "code",
			form:       url.Values{"long": {"https://github.com/org"}, "owner": {"foo@example.com"}},
			wantStatus: http.StatusOK,
			wantLong:   "https://github.com/org",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			form := url.Values{"short": {tt.short}}
			for k, values := range tt.form {
				form[k] = values
			}
			token := newShortName
			if _, err := db.Load(tt.short); err == nil {
				token = tt.short
			}
			form.Set("xsrf", xsrftoken.Generate(xsrfKey, "foo@example.com", token))

			r := httptest.NewRequest("POST", "/", strings.NewReader(form.Encode()))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			w := httptest.NewRecorder()
			serveSave(w, r)
			if w.Code != tt.wantStatus {
				t.Fatalf("serveSave(%v) = %d (%s); want %d", form, w.Code, w.Body.String(), tt.wantStatus)
			}
			if tt.wantStatus != http.StatusOK {
				return
			}

			link, err := db.Load(tt.short)
			if err != nil {
				t.Fatal(err)
			}
			if link.Long != tt.wantLong {
				t.Errorf("link %q Long = %q; want %q", tt.short, link.Long, tt.wantLong)
			}
			if link.Pattern != tt.wantPattern {
				t.Errorf("link %q Pattern = %q; want %q", tt.short, link.Pattern, tt.wantPattern)
			}
		})
	}
}

// TestRestoreSnapshot tests that a snapshot written before links had patterns
// restores to links that answer to the same URLs, and that one written since
// is restored as it says.
func TestRestoreSnapshot(t *testing.T) {
	var err error
	db, err = NewSQLiteDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}

	oldSnapshot := LastSnapshot
	LastSnapshot = []byte(`{"Short":"oldest","Long":"http://old/"}
{"Short":"oldest-template","Long":"http://old/{{.Path}}"}
{"Short":"was-static","Long":"http://static/","Dynamic":false}
{"Short":"was-dynamic","Long":"http://dyn/","Dynamic":true}
{"Short":"current","Long":"http://cur/","Pattern":"http://cur/{{.Path}}"}
{"Short":"current-static","Long":"http://cur/","Pattern":""}
`)
	t.Cleanup(func() { LastSnapshot = oldSnapshot })

	if err := restoreLastSnapshot(); err != nil {
		t.Fatal(err)
	}

	for _, tt := range []struct {
		short       string
		wantLong    string
		wantPattern string
	}{
		// Written when every link answered for the paths below it.
		{short: "oldest", wantLong: "http://old/", wantPattern: "http://old/{{.Path}}"},
		{short: "oldest-template", wantPattern: "http://old/{{.Path}}"},
		// Written when a Dynamic flag said so.
		{short: "was-static", wantLong: "http://static/"},
		{short: "was-dynamic", wantLong: "http://dyn/", wantPattern: "http://dyn/{{.Path}}"},
		// Written since patterns.
		{short: "current", wantLong: "http://cur/", wantPattern: "http://cur/{{.Path}}"},
		{short: "current-static", wantLong: "http://cur/"},
	} {
		link, err := db.Load(tt.short)
		if err != nil {
			t.Errorf("db.Load(%q): %v", tt.short, err)
			continue
		}
		if link.Long != tt.wantLong || link.Pattern != tt.wantPattern {
			t.Errorf("restored %q = (Long %q, Pattern %q); want (%q, %q)", tt.short, link.Long, link.Pattern, tt.wantLong, tt.wantPattern)
		}
	}
}

// TestAuditLog tests that every change to a link is recorded, with who made
// it and what the link looked like before, and that the link itself remembers
// who saved it last.
func TestAuditLog(t *testing.T) {
	var err error
	db, err = NewSQLiteDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	oldWriter := auditWriter
	auditWriter = &buf
	t.Cleanup(func() { auditWriter = oldWriter })

	// So that one user can edit another's link, as they can in the deployment.
	oldOpenLinks := *openLinks
	*openLinks = true
	t.Cleanup(func() { *openLinks = oldOpenLinks })

	as := func(login string) func(*http.Request) (user, error) {
		return func(*http.Request) (user, error) { return user{login: login}, nil }
	}
	post := func(t *testing.T, path string, form url.Values, login string) {
		t.Helper()
		oldCurrentUser := currentUser
		currentUser = as(login)
		defer func() { currentUser = oldCurrentUser }()

		r := httptest.NewRequest("POST", path, strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		serveHandler().ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("POST %s as %s = %d (%s); want 200", path, login, w.Code, w.Body.String())
		}
	}
	nextEntry := func(t *testing.T) auditEntry {
		t.Helper()
		var entry auditEntry
		if err := json.NewDecoder(&buf).Decode(&entry); err != nil {
			t.Fatalf("decoding audit line: %v", err)
		}
		if entry.Service != "golink" || entry.Status != "info" || entry.Timestamp.IsZero() {
			t.Errorf("audit entry missing collector fields: %+v", entry)
		}
		return entry
	}

	// Create.
	post(t, "/", url.Values{
		"short": {"lk"}, "long": {"http://before/"},
		"xsrf": {xsrftoken.Generate(xsrfKey, "amelie@example.com", newShortName)},
	}, "amelie@example.com")

	entry := nextEntry(t)
	if entry.Action != "create" || entry.Short != "lk" || entry.User != "amelie@example.com" {
		t.Errorf("create entry = %+v", entry)
	}
	if entry.Link == nil || entry.Link.Long != "http://before/" {
		t.Errorf("create entry link = %+v", entry.Link)
	}
	if entry.Previous != nil {
		t.Errorf("create entry has a previous state: %+v", entry.Previous)
	}
	if link, err := db.Load("lk"); err != nil {
		t.Fatal(err)
	} else if link.LastEditBy != "amelie@example.com" {
		t.Errorf("LastEditBy after create = %q; want amelie@example.com", link.LastEditBy)
	}

	// Update, by somebody else, which the open-links model allows.
	post(t, "/", url.Values{
		"short": {"lk"}, "long": {"http://after/"}, "owner": {"amelie@example.com"},
		"xsrf": {xsrftoken.Generate(xsrfKey, "bob@example.com", "lk")},
	}, "bob@example.com")

	entry = nextEntry(t)
	if entry.Action != "update" || entry.User != "bob@example.com" {
		t.Errorf("update entry = %+v", entry)
	}
	if entry.Link == nil || entry.Link.Long != "http://after/" {
		t.Errorf("update entry link = %+v", entry.Link)
	}
	if entry.Previous == nil || entry.Previous.Long != "http://before/" {
		t.Errorf("update entry previous = %+v", entry.Previous)
	}
	if link, err := db.Load("lk"); err != nil {
		t.Fatal(err)
	} else if link.LastEditBy != "bob@example.com" {
		t.Errorf("LastEditBy after update = %q; want bob@example.com", link.LastEditBy)
	} else if link.Owner != "amelie@example.com" {
		t.Errorf("owner after another user's edit = %q; want amelie@example.com", link.Owner)
	}

	// Delete.
	post(t, "/.delete/lk", url.Values{
		"xsrf": {xsrftoken.Generate(xsrfKey, "bob@example.com", "lk")},
	}, "bob@example.com")

	entry = nextEntry(t)
	if entry.Action != "delete" || entry.Short != "lk" || entry.User != "bob@example.com" {
		t.Errorf("delete entry = %+v", entry)
	}
	if entry.Link == nil || entry.Link.Long != "http://after/" {
		t.Errorf("delete entry link = %+v", entry.Link)
	}

	if buf.Len() != 0 {
		t.Errorf("audit log has %d unread bytes: %s", buf.Len(), buf.String())
	}
}

func TestWithScheme(t *testing.T) {
	tests := []struct {
		name string
		dest string
		want string
	}{
		{name: "a bare host and path", dest: "g.co/test", want: "https://g.co/test"},
		{name: "a bare host", dest: "app.hibob.com", want: "https://app.hibob.com"},
		{name: "https already", dest: "https://g.co/test", want: "https://g.co/test"},
		{name: "http already, which is left alone", dest: "http://internal-box/admin", want: "http://internal-box/admin"},
		{name: "an upper-case scheme", dest: "HTTPS://G.CO/", want: "HTTPS://G.CO/"},
		{name: "nothing at all", dest: "", want: ""},

		// A destination beginning with "/" aliases another link, which
		// resolveLink follows on purpose.
		{name: "an alias of another link", dest: "/meet", want: "/meet"},

		// A template decides its own scheme when it is expanded.
		{name: "a template from the start", dest: "{{if .Path}}https://a/{{else}}https://b/{{end}}", want: "{{if .Path}}https://a/{{else}}https://b/{{end}}"},
		{name: "a pattern with a bare host", dest: "g.co/{{.Path}}", want: "https://g.co/{{.Path}}"},

		// A colon is not enough to call something a scheme.
		{name: "a host and a port", dest: "localhost:8080/foo", want: "https://localhost:8080/foo"},
		{name: "a host and a port, nothing after", dest: "internal:9000", want: "https://internal:9000"},
		{name: "a scheme that is not http", dest: "mailto:someone@example.com", want: "mailto:someone@example.com"},
		{name: "a scheme before a digit", dest: "mailto:2fa@example.com", want: "mailto:2fa@example.com"},
		{name: "an application scheme", dest: "slack://channel?id=1", want: "slack://channel?id=1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := withScheme(tt.dest); got != tt.want {
				t.Errorf("withScheme(%q) = %q; want %q", tt.dest, got, tt.want)
			}
		})
	}
}

func TestSlackText(t *testing.T) {
	tests := []struct {
		name  string
		entry auditEntry
		want  string
	}{
		{
			name: "a link created",
			entry: auditEntry{
				Action: "create", Short: "aws", User: "amelie@example.com",
				Link: &auditLink{Long: "https://aws.example.com/", Owner: "amelie@example.com"},
			},
			want: "*create* <http://go/.detail/aws|go/aws> by amelie@example.com\nhttps://aws.example.com/",
		},
		{
			name: "a destination changed",
			entry: auditEntry{
				Action: "update", Short: "aws", User: "bob@example.com",
				Link:     &auditLink{Long: "https://new.example.com/"},
				Previous: &auditLink{Long: "https://old.example.com/"},
			},
			want: "*update* <http://go/.detail/aws|go/aws> by bob@example.com\nhttps://new.example.com/\n_was_ https://old.example.com/",
		},
		{
			// The ampersands of a real pattern are markup to Slack.
			name: "a pattern beside a destination",
			entry: auditEntry{
				Action: "create", Short: "code", User: "amelie@example.com",
				Link: &auditLink{Long: "https://github.com/org", Pattern: "https://github.com/search?q=a&type=code"},
			},
			want: "*create* <http://go/.detail/code|go/code> by amelie@example.com\nhttps://github.com/org (pattern https://github.com/search?q=a&amp;type=code)",
		},
		{
			name: "a link locked",
			entry: auditEntry{
				Action: "update", Short: "hr", User: "admin@example.com",
				Link:     &auditLink{Long: "https://hr.example.com/", Locked: true},
				Previous: &auditLink{Long: "https://hr.example.com/"},
			},
			want: "*update* <http://go/.detail/hr|go/hr> by admin@example.com\nhttps://hr.example.com/\n_locked_",
		},
		{
			name: "a link deleted",
			entry: auditEntry{
				Action: "delete", Short: "old", User: "bob@example.com",
				Link: &auditLink{Long: "https://gone.example.com/"},
			},
			want: "*delete* <http://go/.detail/old|go/old> by bob@example.com\nhttps://gone.example.com/",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := slackText(tt.entry); got != tt.want {
				t.Errorf("slackText():\n got %q\nwant %q", got, tt.want)
			}
		})
	}
}

// TestWebhook tests that a change to a link reaches the webhook, in the shape
// the format asks for, and that a webhook which fails does not affect the save
// that caused it.
func TestWebhook(t *testing.T) {
	var err error
	db, err = NewSQLiteDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}

	posted := make(chan string, 4)
	status := http.StatusOK
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		posted <- string(body)
		w.WriteHeader(status)
	}))
	t.Cleanup(server.Close)

	oldURL, oldFormat := *webhookURL, *webhookFormat
	*webhookURL = server.URL
	t.Cleanup(func() { *webhookURL, *webhookFormat = oldURL, oldFormat })

	create := func(t *testing.T, short string) {
		t.Helper()
		form := url.Values{
			"short": {short}, "long": {"https://example.com/" + short},
			"xsrf": {xsrftoken.Generate(xsrfKey, "foo@example.com", newShortName)},
		}
		r := httptest.NewRequest("POST", "/", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		serveSave(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("serveSave = %d (%s); want 200", w.Code, w.Body.String())
		}
	}
	receive := func(t *testing.T) string {
		t.Helper()
		select {
		case body := <-posted:
			return body
		case <-time.After(5 * time.Second):
			t.Fatal("nothing reached the webhook")
			return ""
		}
	}

	t.Run("slack", func(t *testing.T) {
		*webhookFormat = "slack"
		startWebhook()
		t.Cleanup(stopWebhook)

		create(t, "one")
		var body struct{ Text string }
		if err := json.Unmarshal([]byte(receive(t)), &body); err != nil {
			t.Fatalf("the webhook body is not what Slack expects: %v", err)
		}
		for _, want := range []string{"*create*", "go/one", "foo@example.com", "https://example.com/one"} {
			if !strings.Contains(body.Text, want) {
				t.Errorf("the message %q does not mention %q", body.Text, want)
			}
		}
	})

	t.Run("json", func(t *testing.T) {
		*webhookFormat = "json"
		startWebhook()
		t.Cleanup(stopWebhook)

		create(t, "two")
		var entry auditEntry
		if err := json.Unmarshal([]byte(receive(t)), &entry); err != nil {
			t.Fatalf("the webhook body is not an audit event: %v", err)
		}
		if entry.Action != "create" || entry.Short != "two" || entry.User != "foo@example.com" {
			t.Errorf("the event posted was %+v", entry)
		}
	})

	t.Run("a webhook that refuses does not fail the save", func(t *testing.T) {
		*webhookFormat = "slack"
		status = http.StatusInternalServerError
		t.Cleanup(func() { status = http.StatusOK })
		startWebhook()
		t.Cleanup(stopWebhook)

		create(t, "three") // fails the test itself if the save does not return 200
		receive(t)
		if _, err := db.Load("three"); err != nil {
			t.Errorf("the link was not saved though only the webhook failed: %v", err)
		}
	})

	t.Run("no webhook configured", func(t *testing.T) {
		stopWebhook() // webhookEvents is nil, as it is when -webhook-url is unset
		create(t, "four")
		select {
		case body := <-posted:
			t.Errorf("something was posted with no webhook running: %s", body)
		case <-time.After(200 * time.Millisecond):
		}
	})
}

func TestLoadConfig(t *testing.T) {
	// A set of flags of the shapes a real configuration would set, kept apart
	// from the process's own so that this cannot disturb another test.
	newFlags := func() (*flag.FlagSet, *bool, *string, *int) {
		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		return fs, fs.Bool("open-links", false, ""), fs.String("sqlitedb", "", ""), fs.Int("port", 0, "")
	}

	t.Run("a file of settings", func(t *testing.T) {
		fs, openLinks, sqlitedb, port := newFlags()
		path := writeConfig(t, `{
			// Comments and trailing commas are allowed, which is the whole
			// reason this is hujson rather than json.
			"open-links": true,
			"sqlitedb": "/home/nonroot/golink.db",
			"port": 8080,
		}`)
		if err := loadConfig(fs, path); err != nil {
			t.Fatal(err)
		}
		if !*openLinks || *sqlitedb != "/home/nonroot/golink.db" || *port != 8080 {
			t.Errorf("after loadConfig: open-links=%v sqlitedb=%q port=%d", *openLinks, *sqlitedb, *port)
		}
	})

	t.Run("the command line wins", func(t *testing.T) {
		fs, openLinks, sqlitedb, _ := newFlags()
		if err := fs.Parse([]string{"-sqlitedb", "/tmp/other.db"}); err != nil {
			t.Fatal(err)
		}
		path := writeConfig(t, `{"open-links": true, "sqlitedb": "/home/nonroot/golink.db"}`)
		if err := loadConfig(fs, path); err != nil {
			t.Fatal(err)
		}
		if *sqlitedb != "/tmp/other.db" {
			t.Errorf("sqlitedb = %q; want the value from the command line", *sqlitedb)
		}
		if !*openLinks {
			t.Error("open-links was not taken from the file, though the command line did not name it")
		}
	})

	for _, tt := range []struct {
		name    string
		config  string
		wantErr string
	}{
		{name: "a misspelled option", config: `{"open-lynx": true}`, wantErr: `no such option "open-lynx"`},
		{name: "a value of the wrong shape", config: `{"open-links": "yes please"}`, wantErr: `option "open-links"`},
		{name: "a setting with no value", config: `{"sqlitedb": null}`, wantErr: "has no value"},
		{name: "a setting that is a list", config: `{"sqlitedb": ["a", "b"]}`, wantErr: "not a setting"},
		{name: "a file naming another file", config: `{"config": "other.hujson"}`, wantErr: "cannot name another one"},
		{name: "not a configuration at all", config: `nonsense`, wantErr: ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fs, _, _, _ := newFlags()
			fs.String("config", "", "")
			err := loadConfig(fs, writeConfig(t, tt.config))
			if err == nil {
				t.Fatalf("loadConfig(%s) succeeded; want an error", tt.config)
			}
			if tt.wantErr != "" && !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("loadConfig(%s) = %v; want it to mention %q", tt.config, err, tt.wantErr)
			}
		})
	}

	t.Run("a file that is not there", func(t *testing.T) {
		fs, _, _, _ := newFlags()
		if err := loadConfig(fs, filepath.Join(t.TempDir(), "absent.hujson")); err == nil {
			t.Error("loadConfig of a missing file succeeded; want an error")
		}
	})
}

// writeConfig puts a configuration file in a temporary directory and returns
// its path.
func writeConfig(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "golink.hujson")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCanLockLink(t *testing.T) {
	var (
		link  = &Link{Short: "a", Owner: "foo@example.com"}
		owner = user{login: "foo@example.com"}
		other = user{login: "bar@example.com"}
		admin = user{login: "bar@example.com", isAdmin: true}
	)

	tests := []struct {
		name         string
		link         *Link
		user         user
		openLinks    bool
		ownerCanLock bool
		want         bool
	}{
		// Locking is an admin decision by default.
		{name: "admin", link: link, user: admin, openLinks: true, want: true},
		{name: "owner", link: link, user: owner, openLinks: true, want: false},
		{name: "anybody else", link: link, user: other, openLinks: true, want: false},
		{name: "a link being created", link: nil, user: owner, openLinks: true, want: false},
		{name: "a link being created, by an admin", link: nil, user: admin, openLinks: true, want: true},

		// With -owner-can-lock the owner may do it too, and still nobody else.
		{name: "owner can lock: admin", link: link, user: admin, openLinks: true, ownerCanLock: true, want: true},
		{name: "owner can lock: owner", link: link, user: owner, openLinks: true, ownerCanLock: true, want: true},
		{name: "owner can lock: anybody else", link: link, user: other, openLinks: true, ownerCanLock: true, want: false},

		// A lock says nothing that is not already true without -open-links.
		{name: "no open links: admin", link: link, user: admin, want: false},
		{name: "no open links: owner", link: link, user: owner, ownerCanLock: true, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oldOpenLinks, oldOwnerCanLock := *openLinks, *ownerCanLock
			*openLinks, *ownerCanLock = tt.openLinks, tt.ownerCanLock
			t.Cleanup(func() { *openLinks, *ownerCanLock = oldOpenLinks, oldOwnerCanLock })

			if got := canLockLink(tt.link, tt.user); got != tt.want {
				t.Errorf("canLockLink(%v, %v) = %v; want %v", tt.link, tt.user, got, tt.want)
			}
		})
	}
}

func TestCanEditLink(t *testing.T) {
	var (
		unlocked = &Link{Short: "a", Owner: "foo@example.com"}
		locked   = &Link{Short: "b", Owner: "foo@example.com", Locked: true}
		unowned  = &Link{Short: "c"}

		owner = user{login: "foo@example.com"}
		other = user{login: "bar@example.com"}
		admin = user{login: "bar@example.com", isAdmin: true}
	)

	tests := []struct {
		name      string
		link      *Link
		user      user
		openLinks bool
		readonly  bool
		want      bool
	}{
		// The default permission model: a link belongs to its owner. Tests run
		// in dev mode, where userExists always reports that the owner exists.
		{name: "new link", link: nil, user: other, want: true},
		{name: "unowned link", link: unowned, user: other, want: true},
		{name: "another user's link", link: unlocked, user: other, want: false},
		{name: "another user's link, admin", link: unlocked, user: admin, want: true},
		{name: "own link", link: unlocked, user: owner, want: true},

		// With -open-links, only a locked link belongs to its owner.
		{name: "open links: new link", link: nil, user: other, openLinks: true, want: true},
		{name: "open links: unowned link", link: unowned, user: other, openLinks: true, want: true},
		{name: "open links: another user's unlocked link", link: unlocked, user: other, openLinks: true, want: true},
		{name: "open links: own unlocked link", link: unlocked, user: owner, openLinks: true, want: true},
		{name: "open links: another user's locked link", link: locked, user: other, openLinks: true, want: false},
		{name: "open links: own locked link", link: locked, user: owner, openLinks: true, want: true},
		{name: "open links: another user's locked link, admin", link: locked, user: admin, openLinks: true, want: true},

		// Read-only mode refuses every edit, in either model.
		{name: "readonly, own link", link: unlocked, user: owner, readonly: true, want: false},
		{name: "readonly, admin", link: unlocked, user: admin, readonly: true, want: false},
		{name: "readonly, new link", link: nil, user: owner, readonly: true, want: false},
		{name: "readonly, open links, unlocked link", link: unlocked, user: other, openLinks: true, readonly: true, want: false},
		{name: "readonly, open links, own link", link: unlocked, user: owner, openLinks: true, readonly: true, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oldReadonly := *readonly
			*readonly = tt.readonly
			t.Cleanup(func() { *readonly = oldReadonly })

			oldOpenLinks := *openLinks
			*openLinks = tt.openLinks
			t.Cleanup(func() { *openLinks = oldOpenLinks })

			if got := canEditLink(context.Background(), tt.link, tt.user); got != tt.want {
				t.Errorf("canEditLink(%v, %v) = %v; want %v", tt.link, tt.user, got, tt.want)
			}
		})
	}
}

func TestServeDelete(t *testing.T) {
	var err error
	db, err = NewSQLiteDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.Save(&Link{Short: "a", Owner: "a@example.com"})
	db.Save(&Link{Short: "foo", Owner: "foo@example.com"})
	db.Save(&Link{Short: "link-owned-by-tagged-devices", Long: "/before", Owner: "tagged-devices"})
	db.Save(&Link{Short: "b", Owner: "a@example.com"})
	db.Save(&Link{Short: "locked-link", Owner: "a@example.com", Locked: true})
	db.Save(&Link{Short: "locked-link2", Owner: "a@example.com", Locked: true})

	xsrf := func(short string) string {
		return xsrftoken.Generate(xsrfKey, "foo@example.com", short)
	}

	tests := []struct {
		name        string
		short       string
		xsrf        string
		openLinks   bool
		currentUser func(*http.Request) (user, error)
		wantStatus  int
	}{
		{
			name:       "missing short",
			short:      "",
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "nonexistent link",
			short:      "does-not-exist",
			wantStatus: http.StatusNotFound,
		},
		{
			name:       "another user's link",
			short:      "a",
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "open links: another user's unlocked link",
			short:      "b",
			xsrf:       xsrf("b"),
			openLinks:  true,
			wantStatus: http.StatusOK,
		},
		{
			name:       "open links: another user's locked link",
			short:      "locked-link",
			xsrf:       xsrf("locked-link"),
			openLinks:  true,
			wantStatus: http.StatusForbidden,
		},
		{
			name:        "open links: admin can delete another user's locked link",
			short:       "locked-link2",
			xsrf:        xsrf("locked-link2"),
			openLinks:   true,
			currentUser: func(*http.Request) (user, error) { return user{login: "foo@example.com", isAdmin: true}, nil },
			wantStatus:  http.StatusOK,
		},
		{
			name:       "allow deleting link owned by tagged-devices",
			short:      "link-owned-by-tagged-devices",
			xsrf:       xsrf("link-owned-by-tagged-devices"),
			wantStatus: http.StatusOK,
		},
		{
			name:        "admin can delete another user's link",
			short:       "a",
			currentUser: func(*http.Request) (user, error) { return user{login: "foo@example.com", isAdmin: true}, nil },
			xsrf:        xsrf("a"),
			wantStatus:  http.StatusOK,
		},
		{
			name:       "invalid xsrf",
			short:      "foo",
			xsrf:       xsrf("invalid"),
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "valid xsrf",
			short:      "foo",
			xsrf:       xsrf("foo"),
			wantStatus: http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.currentUser != nil {
				oldCurrentUser := currentUser
				currentUser = tt.currentUser
				t.Cleanup(func() {
					currentUser = oldCurrentUser
				})
			}

			oldOpenLinks := *openLinks
			*openLinks = tt.openLinks
			t.Cleanup(func() { *openLinks = oldOpenLinks })

			r := httptest.NewRequest("POST", "/.delete/"+tt.short, strings.NewReader(url.Values{
				"xsrf": {tt.xsrf},
			}.Encode()))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			w := httptest.NewRecorder()
			serveDelete(w, r)
			t.Logf("response body: %v", w.Body.String())
			if w.Code != tt.wantStatus {
				t.Errorf("serveDelete(%q) = %d; want %d", tt.short, w.Code, tt.wantStatus)
			}
		})
	}
}

func TestServeExport(t *testing.T) {
	clock := tstest.NewClock(tstest.ClockOpts{
		Start: time.Date(2022, 06, 02, 1, 2, 3, 4, time.UTC),
	})

	var err error
	db, err = NewSQLiteDB(":memory:")
	db.clock = clock
	if err != nil {
		t.Fatal(err)
	}
	db.Save(&Link{Short: "a", Owner: "a@example.com"})
	db.Save(&Link{Short: "foo", Owner: "foo@example.com"})
	db.Save(&Link{Short: "link-owned-by-tagged-devices", Long: "/before", Owner: "tagged-devices"})

	click := func(id string) {
		r := httptest.NewRequest("GET", "/"+id, nil)
		w := httptest.NewRecorder()
		serveHandler().ServeHTTP(w, r)
	}
	initStats()
	click("a")
	click("foo")
	click("foo")
	flushStats()
	clock.Advance(3 * time.Minute)
	click("a")

	// export links
	r := httptest.NewRequest("GET", "/.export", nil)
	w := httptest.NewRecorder()
	serveHandler().ServeHTTP(w, r)

	if want := http.StatusOK; w.Code != want {
		t.Errorf("serveExport = %d; want %d", w.Code, want)
	}
	wantOutput := `{"Short":"a","Long":"","Pattern":"","Created":"0001-01-01T00:00:00Z","LastEdit":"0001-01-01T00:00:00Z","Owner":"a@example.com"}
{"Short":"foo","Long":"","Pattern":"","Created":"0001-01-01T00:00:00Z","LastEdit":"0001-01-01T00:00:00Z","Owner":"foo@example.com"}
{"Short":"link-owned-by-tagged-devices","Long":"/before","Pattern":"","Created":"0001-01-01T00:00:00Z","LastEdit":"0001-01-01T00:00:00Z","Owner":"tagged-devices"}
`
	if got := w.Body.String(); got != wantOutput {
		t.Errorf("serveExport = %v; want %v", got, wantOutput)
	}

	// export links stats
	r = httptest.NewRequest("GET", "/.export-stats", nil)
	w = httptest.NewRecorder()
	serveHandler().ServeHTTP(w, r)

	if want := http.StatusOK; w.Code != want {
		t.Errorf("serveExportStats = %d; want %d", w.Code, want)
	}
	wantOutput = `a,1654131723,1
foo,1654131723,2
a,1654131903,1
`
	if got := w.Body.String(); got != wantOutput {
		t.Errorf("serveExportStats = %v; want %v", got, wantOutput)
	}
}

func TestReadOnlyMode(t *testing.T) {
	var err error
	db, err = NewSQLiteDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.Save(&Link{Short: "who", Long: "http://who/"})

	oldReadOnly := readonly
	readonly = ptr.To(true)
	defer func() { readonly = oldReadOnly }()

	// resolving link should succeed
	r := httptest.NewRequest("GET", "/who", nil)
	w := httptest.NewRecorder()
	serveHandler().ServeHTTP(w, r)
	if want := http.StatusFound; w.Code != want {
		t.Errorf("serveHandler() = %d; want %d", w.Code, want)
	}
	wantLocation := "http://who/"
	if location := w.Header().Get("Location"); location != wantLocation {
		t.Errorf("serveHandler() location = %v; want %v", location, wantLocation)
	}

	// updating link should fail
	r = httptest.NewRequest("POST", "/", nil)
	w = httptest.NewRecorder()
	serveHandler().ServeHTTP(w, r)
	if want := http.StatusMethodNotAllowed; w.Code != want {
		t.Errorf("serveHandler() = %d; want %d", w.Code, want)
	}

	// deleting link should fail
	r = httptest.NewRequest("POST", "/.delete/who", nil)
	w = httptest.NewRecorder()
	serveHandler().ServeHTTP(w, r)
	if want := http.StatusMethodNotAllowed; w.Code != want {
		t.Errorf("serveHandler() = %d; want %d", w.Code, want)
	}
}

func TestExpandLink(t *testing.T) {
	tests := []struct {
		name      string    // test name
		long      string    // long URL for golink
		now       time.Time // current time
		user      string    // current user resolving link
		query     string    // query string
		remainder string    // remainder of URL path after golink name
		wantErr   bool      // whether we expect an error
		want      string    // expected redirect URL
	}{
		{
			name: "dont-mangle-escapes",
			long: "http://host.com/foo%2f/bar",
			want: "http://host.com/foo%2f/bar",
		},
		{
			name:      "dont-mangle-escapes-and-remainder",
			long:      "http://host.com/foo%2f/bar",
			remainder: "extra",
			want:      "http://host.com/foo%2f/bar/extra",
		},
		{
			name:      "remainder-insert-slash",
			long:      "http://host.com/foo",
			remainder: "extra",
			want:      "http://host.com/foo/extra",
		},
		{
			name:      "remainder-long-as-trailing-slash",
			long:      "http://host.com/foo/",
			remainder: "extra",
			want:      "http://host.com/foo/extra",
		},
		{
			name: "var-expansions-time",
			long: `https://roamresearch.com/#/app/ts-corp/page/{{.Now.Format "01-02-2006"}}`,
			want: "https://roamresearch.com/#/app/ts-corp/page/06-02-2022",
			now:  time.Date(2022, 06, 02, 1, 2, 3, 4, time.UTC),
		},
		{
			name: "var-expansions-user",
			long: `http://host.com/{{.User}}`,
			user: "foo@example.com",
			want: "http://host.com/foo@example.com",
		},
		{
			name:    "var-expansions-no-user",
			long:    `http://host.com/{{.User}}`,
			wantErr: true,
		},
		{
			name:    "unknown-field",
			long:    `http://host.com/{{.Foo}}`,
			wantErr: true,
		},
		{
			name: "template-no-path",
			long: "https://calendar.google.com/{{with .Path}}calendar/embed?mode=week&src={{.}}@tailscale.com{{end}}",
			want: "https://calendar.google.com/",
		},
		{
			name:      "template-with-path",
			long:      "https://calendar.google.com/{{with .Path}}calendar/embed?mode=week&src={{.}}@tailscale.com{{end}}",
			remainder: "amelie",
			want:      "https://calendar.google.com/calendar/embed?mode=week&src=amelie@tailscale.com",
		},
		{
			name:      "template-with-pathescape-func",
			long:      "http://host.com/{{PathEscape .Path}}",
			remainder: "a/b+c",
			want:      "http://host.com/a%2Fb+c",
		},
		{
			name:      "template-with-queryescape-func",
			long:      "http://host.com/{{QueryEscape .Path}}",
			remainder: "a/b+c",
			want:      "http://host.com/a%2Fb%2Bc",
		},
		{
			name:      "template-with-trimprefix-func",
			long:      `http://host.com/{{TrimPrefix .Path "BUG-"}}`,
			remainder: "BUG-123",
			want:      "http://host.com/123",
		},
		{
			name:      "template-with-trimsuffix-func",
			long:      `http://host.com/{{TrimSuffix .Path "/"}}`,
			remainder: "a/",
			want:      "http://host.com/a",
		},
		{
			name:      "template-with-tolower-func",
			long:      `http://host.com/{{ToLower .Path}}`,
			remainder: "BUG-123",
			want:      "http://host.com/bug-123",
		},
		{
			name:      "template-with-toupper-func",
			long:      `http://host.com/{{ToUpper .Path}}`,
			remainder: "bug-123",
			want:      "http://host.com/BUG-123",
		},
		{
			name:      "template-with-match-func",
			long:      `http://host.com/{{if Match "\\d+" .Path}}id/{{.Path}}{{else}}search/{{.Path}}{{end}}`,
			remainder: "123",
			want:      "http://host.com/id/123",
		},
		{
			name:      "template-with-match-func2",
			long:      `http://host.com/{{if Match "\\d+" .Path}}id/{{.Path}}{{else}}search/{{.Path}}{{end}}`,
			remainder: "query",
			want:      "http://host.com/search/query",
		},
		{
			name:      "relative-link",
			long:      `rel`,
			remainder: "a",
			want:      "rel/a",
		},
		{
			name:      "relative-link-with-slash",
			long:      `/rel`,
			remainder: "a",
			want:      "/rel/a",
		},
		{
			name:  "query-string",
			long:  `/rel`,
			query: "a=b",
			want:  "/rel?a=b",
		},
		{
			name:      "path-and-query-string",
			long:      `/rel`,
			remainder: "path",
			query:     "a=b",
			want:      "/rel/path?a=b",
		},
		{
			name:  "combine-query-string",
			long:  `/rel?a=1`,
			query: "a=2&b=2",
			want:  "/rel?a=1&a=2&b=2",
		},
		{
			name:      "template-and-combined-query-string",
			long:      `/rel{{with .Path}}/{{.}}{{end}}?a=1`,
			remainder: "path",
			query:     "b=2",
			want:      "/rel/path?a=1&b=2",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			query, _ := url.ParseQuery(tt.query)
			env := expandEnv{Now: tt.now, Path: tt.remainder, user: tt.user, query: query}
			link, err := expandLink(tt.long, env)
			if (err != nil) != tt.wantErr {
				t.Fatalf("expandLink(%q) returned error %v; want %v", tt.long, err, tt.wantErr)
			}
			var got string
			if link != nil {
				got = link.String()
			}
			if got != tt.want {
				t.Errorf("expandLink(%q) = %q; want %q", tt.long, got, tt.want)
			}
		})
	}
}

func TestResolveLink(t *testing.T) {
	var err error
	db, err = NewSQLiteDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.Save(&Link{Short: "meet", Long: "https://meet.google.com/lookup/", Pattern: "https://meet.google.com/lookup/{{.Path}}"})
	db.Save(&Link{Short: "cs", Pattern: "http://codesearch/{{with .Path}}search?q={{.}}{{end}}"})
	// Aliases forward the rest of the path through a pattern of their own.
	db.Save(&Link{Short: "m", Long: "http://go/meet", Pattern: "http://go/meet/{{.Path}}"})
	db.Save(&Link{Short: "chat", Long: "/meet", Pattern: "/meet/{{.Path}}"})

	tests := []struct {
		link string
		want string
	}{
		{
			link: "meet",
			want: "https://meet.google.com/lookup/",
		},
		{
			link: "meet/foo",
			want: "https://meet.google.com/lookup/foo",
		},
		{
			link: "go/meet/foo",
			want: "https://meet.google.com/lookup/foo",
		},
		{
			link: "http://go/meet/foo",
			want: "https://meet.google.com/lookup/foo",
		},
		{
			// if absolute URL provided, host doesn't actually matter
			link: "http://mygo/meet/foo",
			want: "https://meet.google.com/lookup/foo",
		},
		{
			link: "cs",
			want: "http://codesearch/",
		},
		{
			link: "cs/term",
			want: "http://codesearch/search?q=term",
		},
		{
			// aliased go links with hostname
			link: "m/foo",
			want: "https://meet.google.com/lookup/foo",
		},
		{
			// aliased go links without hostname
			link: "chat/foo",
			want: "https://meet.google.com/lookup/foo",
		},
	}
	for _, tt := range tests {
		name := "golink " + tt.link
		t.Run(name, func(t *testing.T) {
			u := must.Get(url.Parse(tt.link))
			got, err := resolveLink(u)
			if err != nil {
				t.Error(err)
			}
			if got.String() != tt.want {
				t.Errorf("ResolveLink(%q) = %q; want %q", tt.link, got.String(), tt.want)
			}
		})
	}
}

func TestNoHSTSShortDomain(t *testing.T) {
	var err error
	db, err = NewSQLiteDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.Save(&Link{Short: "foobar", Long: "http://foobar/"})

	tests := []struct {
		host       string
		expectHsts bool
	}{
		{
			host:       "go",
			expectHsts: false,
		},
		{
			host:       "go.prawn-universe.ts.net",
			expectHsts: true,
		},
	}
	for _, tt := range tests {
		name := "HSTS: " + tt.host
		t.Run(name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/foobar", nil)
			r.Header.Add("Host", tt.host)

			w := httptest.NewRecorder()
			HSTS(serveHandler()).ServeHTTP(w, r)

			_, found := w.Header()["Strict-Transport-Security"]
			if found != tt.expectHsts {
				t.Errorf("HSTS expectation: domain %s want: %t got: %t", tt.host, tt.expectHsts, found)
			}
		})
	}
}

func TestHTTPSRedirectHandlerWithQuery(t *testing.T) {
	h := redirectHandler("foobar.com")
	r := httptest.NewRequest("GET", "http://example.com/?query=bar", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusFound {
		t.Errorf("got %d; want %d", w.Code, http.StatusFound)
	}
	if w.Header().Get("Location") != "https://foobar.com/?query=bar" {
		t.Errorf("got %q; want %q", w.Header().Get("Location"), "https://foobar.com/?query=bar")
	}
}

func TestServeSearch(t *testing.T) {
	var err error
	db, err = NewSQLiteDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	links := []*Link{
		{Short: "alpha", Long: "http://alpha/", Owner: "foo@example.com"},
		{Short: "beta", Long: "http://beta/", Owner: "foo@example.com"},
		{Short: "gamma", Long: "http://gamma/", Owner: "bar@example.com"},
		{Short: "delta", Long: "http://delta/", Owner: "FOO@example.com"},
		{Short: "bob", Long: "https://bob.example.com/"},
		{Short: "hi-bob", Long: "https://directory/bob"},
		{Short: "hr", Long: "https://app.hibob.com/"},
		{Short: "ticket", Pattern: "https://bob.example.com/t/{{.Path}}"},
		{Short: "100%", Long: "https://percent/"},
	}
	for _, link := range links {
		if err := db.Save(link); err != nil {
			t.Error(err)
		}
	}

	tests := []struct {
		name            string
		query           string
		wantStatus      int
		wantLocation    string
		wantContains    []string // substrings that should appear in response body
		wantNotContains []string // substrings that should NOT appear in response body
	}{
		{
			name:            "search by owner with multiple links",
			query:           "owner:foo@example.com",
			wantStatus:      http.StatusOK,
			wantContains:    []string{"alpha", "beta", "delta", "3 total"},
			wantNotContains: []string{"gamma"},
		},
		{
			name:         "search by owner case insensitive",
			query:        "owner:FOO@EXAMPLE.COM",
			wantStatus:   http.StatusOK,
			wantContains: []string{"alpha", "beta", "delta"},
		},
		{
			name:            "search by owner with single link",
			query:           "owner:bar@example.com",
			wantStatus:      http.StatusOK,
			wantContains:    []string{"gamma", "1 total"},
			wantNotContains: []string{"alpha", "beta"},
		},
		{
			// A name, a name containing the query, and a destination
			// containing it, all at once.
			name:            "search names and destinations",
			query:           "bob",
			wantStatus:      http.StatusOK,
			wantContains:    []string{"go/bob", "go/hi-bob", "go/hr", "go/ticket", "4 total"},
			wantNotContains: []string{"go/alpha"},
		},
		{
			name:         "search without regard to case",
			query:        "BOB",
			wantStatus:   http.StatusOK,
			wantContains: []string{"go/bob", "go/hr"},
		},
		{
			// Dashes are ignored when resolving a link, so they are ignored
			// when searching for one too.
			name:         "search ignoring dashes",
			query:        "hibob",
			wantStatus:   http.StatusOK,
			wantContains: []string{"go/hi-bob", "go/hr"},
		},
		{
			name:         "search matching a pattern",
			query:        "t/{{.Path}}",
			wantStatus:   http.StatusOK,
			wantContains: []string{"go/ticket", "1 total"},
		},
		{
			// A LIKE wildcard in the query matches itself, not everything.
			name:            "search for a percent sign",
			query:           "100%",
			wantStatus:      http.StatusOK,
			wantContains:    []string{"go/100%", "1 total"},
			wantNotContains: []string{"go/alpha"},
		},
		{
			name:         "no match",
			query:        "nothing-matches-this",
			wantStatus:   http.StatusOK,
			wantContains: []string{"0 total", "No link"},
		},
		{
			name:         "empty query",
			query:        "  ",
			wantStatus:   http.StatusFound,
			wantLocation: "/.all",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/.search?q="+url.QueryEscape(tt.query), nil)
			w := httptest.NewRecorder()
			serveHandler().ServeHTTP(w, r)

			if w.Code != tt.wantStatus {
				t.Errorf("serveSearch(%q) = %d; want %d", tt.query, w.Code, tt.wantStatus)
			}
			if tt.wantLocation != "" {
				if got := w.Header().Get("Location"); got != tt.wantLocation {
					t.Errorf("serveSearch(%q) Location = %q; want %q", tt.query, got, tt.wantLocation)
				}
			}

			body := w.Body.String()
			for _, s := range tt.wantContains {
				if !strings.Contains(body, s) {
					t.Errorf("serveSearch(%q) body missing %q", tt.query, s)
				}
			}
			for _, s := range tt.wantNotContains {
				if strings.Contains(body, s) {
					t.Errorf("serveSearch(%q) body unexpectedly contains %q", tt.query, s)
				}
			}
		})
	}
}

func TestSearchResults(t *testing.T) {
	stats.mu.Lock()
	stats.clicks = ClickStats{"alpha": 3, "beta": 10}
	stats.mu.Unlock()
	t.Cleanup(func() {
		stats.mu.Lock()
		stats.clicks = nil
		stats.mu.Unlock()
	})

	day := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	links := []*Link{
		{Short: "alpha", Owner: "z@example.com", LastEdit: day},
		{Short: "beta", Owner: "a@example.com", LastEdit: day.AddDate(0, 0, 1)},
		{Short: "gamma", Owner: "m@example.com", LastEdit: day.AddDate(0, 0, 2)}, // no recorded clicks; should annotate to 0
	}

	// The default order is alphabetical by short name, with each link
	// annotated with its current click count.
	want := []struct {
		Short     string
		NumClicks int
	}{
		{Short: "alpha", NumClicks: 3},
		{Short: "beta", NumClicks: 10},
		{Short: "gamma", NumClicks: 0},
	}

	got := searchResults(links, "")
	if len(got) != len(want) {
		t.Fatalf("searchResults returned %d results; want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].Short != w.Short || got[i].NumClicks != w.NumClicks {
			t.Errorf("result[%d] = {%q, %d}; want {%q, %d}", i, got[i].Short, got[i].NumClicks, w.Short, w.NumClicks)
		}
	}

	// Every other order, each with the direction that is useful for it.
	for _, tt := range []struct {
		order string
		want  []string
	}{
		{order: "name", want: []string{"alpha", "beta", "gamma"}},
		{order: "owner", want: []string{"beta", "gamma", "alpha"}},
		{order: "clicks", want: []string{"beta", "alpha", "gamma"}},
		{order: "edited", want: []string{"gamma", "beta", "alpha"}},
		{order: "nonsense", want: []string{"alpha", "beta", "gamma"}},
	} {
		got := searchResults(links, tt.order)
		var names []string
		for _, r := range got {
			names = append(names, r.Short)
		}
		if !slices.Equal(names, tt.want) {
			t.Errorf("searchResults order %q = %v; want %v", tt.order, names, tt.want)
		}
	}
}

func TestSuggestLinks(t *testing.T) {
	var err error
	db, err = NewSQLiteDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	for _, short := range []string{"hibob", "hr", "gh", "gh/infra", "gh/infrastructure", "team/jira", "team/github", "wiki"} {
		if err := db.Save(&Link{Short: short, Long: "http://example.com/" + short}); err != nil {
			t.Fatal(err)
		}
	}

	tests := []struct {
		name  string
		short string
		want  []string
	}{
		{name: "no name at all", short: "", want: nil},
		{name: "one letter too many", short: "hibbob", want: []string{"hibob"}},
		{name: "one letter missing", short: "hibo", want: []string{"hibob"}},
		{name: "dashes are ignored, as they are when resolving", short: "hi-bob", want: nil},
		{name: "a name typed short", short: "gh/inf", want: []string{"gh", "gh/infra", "gh/infrastructure"}},
		{name: "siblings under the same name", short: "team/confluence", want: []string{"team/github", "team/jira"}},
		{name: "nothing like it", short: "zzzzzzzz", want: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got []string
			for _, link := range suggestLinks(tt.short) {
				got = append(got, link.Short)
			}
			slices.Sort(got)
			slices.Sort(tt.want)
			if !slices.Equal(got, tt.want) {
				t.Errorf("suggestLinks(%q) = %v; want %v", tt.short, got, tt.want)
			}
		})
	}
}

func TestParseAdvertiseTags(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    []string
		wantErr bool
	}{
		{
			name:  "empty string",
			input: "",
			want:  nil,
		},
		{
			name:  "single tag",
			input: "tag:golink",
			want:  []string{"tag:golink"},
		},
		{
			name:  "multiple tags",
			input: "tag:golink,tag:server",
			want:  []string{"tag:golink", "tag:server"},
		},
		{
			name:  "whitespace trimmed",
			input: " tag:golink , tag:server ",
			want:  []string{"tag:golink", "tag:server"},
		},
		{
			name:  "trailing comma ignored",
			input: "tag:golink,",
			want:  []string{"tag:golink"},
		},
		{
			name:    "missing tag prefix",
			input:   "golink",
			wantErr: true,
		},
		{
			name:    "one valid one invalid",
			input:   "tag:golink,invalid",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseAdvertiseTags(tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseAdvertiseTags(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
			if !tt.wantErr && !slices.Equal(got, tt.want) {
				t.Errorf("parseAdvertiseTags(%q) diff (-want +got):\n%s", tt.input, cmp.Diff(tt.want, got))
			}
		})
	}
}

func TestTrustIdentityHeaders(t *testing.T) {
	tests := []struct {
		name        string
		serviceName string // value to set *serviceName to
		remoteAddr  string
		want        bool
	}{
		{
			name:        "no service node, loopback",
			serviceName: "",
			remoteAddr:  "127.0.0.1:1234",
			want:        false,
		},
		{
			name:        "service mode, loopback",
			serviceName: "svc:golink",
			remoteAddr:  "127.0.0.1:1234",
			want:        true,
		},
		{
			name:        "service mode, non-loopback",
			serviceName: "svc:golink",
			remoteAddr:  "100.64.1.1:1234",
			want:        false,
		},
		{
			name:        "service mode, IPV6 loopback",
			serviceName: "svc:golink",
			remoteAddr:  "[::1]:1234",
			want:        true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tstest.Replace(t, serviceName, tt.serviceName)
			r := httptest.NewRequest("GET", "/", nil)
			r.RemoteAddr = tt.remoteAddr
			if got := trustIdentityHeaders(r); got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestExtractUserFromHeaders(t *testing.T) {
	adminCapMap := tailcfg.PeerCapMap{
		peerCapName: []tailcfg.RawMessage{
			tailcfg.RawMessage(must.Get(json.Marshal(capabilities{Admin: true}))),
		},
	}
	noCapMap := tailcfg.PeerCapMap{}

	tests := []struct {
		name      string
		headers   map[string]string
		whoisFunc func(context.Context, string) (*apitype.WhoIsResponse, error) // mock for localClient.WhoIs
		wantLogin string
		wantAdmin bool
	}{
		{
			name:      "no headers",
			headers:   nil,
			wantLogin: "",
		},
		{
			name:      "login header only, no XFF",
			headers:   map[string]string{"Tailscale-User-Login": "alice@example.com"},
			wantLogin: "alice@example.com",
			wantAdmin: false,
		},
		{
			name: "login header with XFF, peer has admin cap",
			headers: map[string]string{
				"Tailscale-User-Login": "alice@example.com",
				"X-Forwarded-For":      "100.64.1.1",
			},
			whoisFunc: func(_ context.Context, _ string) (*apitype.WhoIsResponse, error) {
				return &apitype.WhoIsResponse{CapMap: adminCapMap}, nil
			},
			wantLogin: "alice@example.com",
			wantAdmin: true,
		},
		{
			name: "login header with XFF, peer has no admin cap",
			headers: map[string]string{
				"Tailscale-User-Login": "alice@example.com",
				"X-Forwarded-For":      "100.64.1.1",
			},
			whoisFunc: func(_ context.Context, _ string) (*apitype.WhoIsResponse, error) {
				return &apitype.WhoIsResponse{CapMap: noCapMap}, nil
			},
			wantLogin: "alice@example.com",
			wantAdmin: false,
		},
		{
			name: "login header with XFF, WhoIs fails",
			headers: map[string]string{
				"Tailscale-User-Login": "alice@example.com",
				"X-Forwarded-For":      "100.64.1.1",
			},
			whoisFunc: func(_ context.Context, _ string) (*apitype.WhoIsResponse, error) {
				return nil, errors.New("peer not found")
			},
			wantLogin: "alice@example.com",
			wantAdmin: false,
		},
		{
			name: "Invalid X-Forwarded-For header",
			headers: map[string]string{
				"Tailscale-User-Login": "alice@example.com",
				"X-Forwarded-For":      "invalid-ip",
			},
			whoisFunc: func(_ context.Context, _ string) (*apitype.WhoIsResponse, error) {
				return &apitype.WhoIsResponse{CapMap: adminCapMap}, nil
			},
			wantLogin: "alice@example.com",
			wantAdmin: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.whoisFunc != nil {
				tstest.Replace(t, &whoisFunc, tt.whoisFunc)
			}

			r := httptest.NewRequest("GET", "/", nil)
			for k, v := range tt.headers {
				r.Header.Set(k, v)
			}
			got := extractUserFromHeaders(r)
			if got.login != tt.wantLogin {
				t.Errorf("login: got %q, want %q", got.login, tt.wantLogin)
			}
			if got.isAdmin != tt.wantAdmin {
				t.Errorf("isAdmin: got %v, want %v", got.isAdmin, tt.wantAdmin)
			}
		})
	}
}
