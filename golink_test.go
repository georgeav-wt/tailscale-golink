// Copyright 2022 Tailscale Inc & Contributors
// SPDX-License-Identifier: BSD-3-Clause

package golink

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
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
			name:       "template in the destination alongside a pattern",
			short:      "both",
			form:       url.Values{"long": {"https://a/{{.Path}}"}, "pattern": {"https://b/{{.Path}}"}},
			wantStatus: http.StatusBadRequest,
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
	}
	for _, link := range links {
		if err := db.Save(link); err != nil {
			t.Error(err)
		}
	}

	tests := []struct {
		name            string
		owner           string
		wantStatus      int
		wantContains    []string // substrings that should appear in response body
		wantNotContains []string // substrings that should NOT appear in response body
	}{
		{
			name:            "search by owner with multiple links",
			owner:           "foo@example.com",
			wantStatus:      http.StatusOK,
			wantContains:    []string{"alpha", "beta", "delta", "3 total"},
			wantNotContains: []string{"gamma"},
		},
		{
			name:         "search by owner case insensitive",
			owner:        "FOO@EXAMPLE.COM",
			wantStatus:   http.StatusOK,
			wantContains: []string{"alpha", "beta", "delta"},
		},
		{
			name:            "search by owner with single link",
			owner:           "bar@example.com",
			wantStatus:      http.StatusOK,
			wantContains:    []string{"gamma", "1 total"},
			wantNotContains: []string{"alpha", "beta"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			testURL := "/.search?q=owner:" + url.QueryEscape(tt.owner)
			r := httptest.NewRequest("GET", testURL, nil)
			w := httptest.NewRecorder()
			serveHandler().ServeHTTP(w, r)

			if w.Code != tt.wantStatus {
				t.Errorf("serveSearch(owner=%q) = %d; want %d", tt.owner, w.Code, tt.wantStatus)
			}

			body := w.Body.String()
			for _, s := range tt.wantContains {
				if !strings.Contains(body, s) {
					t.Errorf("serveSearch(owner=%q) body missing %q", tt.owner, s)
				}
			}
			for _, s := range tt.wantNotContains {
				if strings.Contains(body, s) {
					t.Errorf("serveSearch(owner=%q) body unexpectedly contains %q", tt.owner, s)
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

	links := []*Link{
		{Short: "alpha"},
		{Short: "beta"},
		{Short: "gamma"}, // no recorded clicks; should annotate to 0
	}

	// Expect the historical alphabetical ordering by short name, with each
	// link annotated with its current click count.
	want := []struct {
		Short     string
		NumClicks int
	}{
		{Short: "alpha", NumClicks: 3},
		{Short: "beta", NumClicks: 10},
		{Short: "gamma", NumClicks: 0},
	}

	got := searchResults(links)
	if len(got) != len(want) {
		t.Fatalf("searchResults returned %d results; want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].Short != w.Short || got[i].NumClicks != w.NumClicks {
			t.Errorf("result[%d] = {%q, %d}; want {%q, %d}", i, got[i].Short, got[i].NumClicks, w.Short, w.NumClicks)
		}
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
