// Copyright 2022 Tailscale Inc & Contributors
// SPDX-License-Identifier: BSD-3-Clause

package golink

import (
	"os"
	"path"
	"slices"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

// testMySQLDSNEnv names a MySQL database for the storage tests to run against
// instead of SQLite. Without it they run against SQLite only, so that the
// suite needs no server; with it the same tests check that every statement is
// read the same way by both databases, which is the property db.go rests on.
//
//	GOLINK_TEST_MYSQL_DSN='golink:golink@tcp(127.0.0.1:3306)/golink_test' go test ./...
//
// The tests empty the database first, so point it at one kept for testing. They
// share it, and can do so because tests in a package run one at a time: do not
// add t.Parallel to one that calls newTestDB.
const testMySQLDSNEnv = "GOLINK_TEST_MYSQL_DSN"

// newTestDB returns an empty database for a test to use.
func newTestDB(t *testing.T) *DB {
	t.Helper()

	dsn := os.Getenv(testMySQLDSNEnv)
	if dsn == "" {
		db, err := NewSQLiteDB(path.Join(t.TempDir(), "links.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.db.Close() })
		return db
	}

	db, err := NewMySQLDB(dsn)
	if err != nil {
		t.Fatalf("connecting to the MySQL database in $%s: %v", testMySQLDSNEnv, err)
	}
	t.Cleanup(func() { db.db.Close() })

	// Every test wants an empty database, and this one is not thrown away
	// between tests the way a temporary file is.
	for _, table := range []string{"Links", "Admins", "Stats"} {
		if _, err := db.db.Exec("DROP TABLE IF EXISTS " + table); err != nil {
			t.Fatal(err)
		}
	}
	if err := execSchema(db.db, db.dialect); err != nil {
		t.Fatal(err)
	}
	return db
}

// Test saving, loading, and deleting links.
func Test_DB_SaveLoadDeleteLinks(t *testing.T) {
	db := newTestDB(t)

	links := []*Link{
		{Short: "short", Long: "long"},
		{Short: "Foo.Bar", Long: "long"},
		{Short: "locked", Long: "long", Owner: "a@example.com", Locked: true},
		{Short: "dynamic", Long: "long", Pattern: "long/{{.Path}}"},
	}

	for _, link := range links {
		if err := db.Save(link); err != nil {
			t.Error(err)
		}
		got, err := db.Load(link.Short)
		if err != nil {
			t.Error(err)
		}

		if !cmp.Equal(got, link) {
			t.Errorf("db save and load got %v, want %v", *got, *link)
		}
	}

	got, err := db.LoadAll()
	if err != nil {
		t.Error(err)
	}

	sortLinks := cmpopts.SortSlices(func(a, b *Link) bool {
		return a.Short < b.Short
	})
	if !cmp.Equal(got, links, sortLinks) {
		t.Errorf("db.LoadAll got %v, want %v", got, links)
	}

	for _, link := range links {
		if err := db.Delete(link.Short); err != nil {
			t.Error(err)
		}
	}

	got, err = db.LoadAll()
	if err != nil {
		t.Error(err)
	}
	want := []*Link(nil)
	if !cmp.Equal(got, want) {
		t.Errorf("db.LoadAll got %v, want %v", got, want)
	}
}

// Test that the Admins table grants admin rights to both users and groups,
// case-insensitively.
func Test_DB_IsAdmin(t *testing.T) {
	db := newTestDB(t)

	// Admins are managed by hand, so add the rows the way an operator would.
	if _, err := db.db.Exec(`INSERT INTO Admins (Name) VALUES ('Amelie@example.com')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`INSERT INTO Admins (Name) VALUES ('group:Eng@example.com')`); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		login  string
		groups []string
		want   bool
	}{
		{name: "listed user", login: "Amelie@example.com", want: true},
		{name: "listed user, different case", login: "amelie@example.com", want: true},
		{name: "unlisted user", login: "someone@example.com", want: false},
		{name: "no login or groups", want: false},
		{name: "listed group", login: "someone@example.com", groups: []string{"eng@example.com"}, want: true},
		{name: "listed group among several", login: "someone@example.com", groups: []string{"sales@example.com", "ENG@example.com"}, want: true},
		{name: "unlisted groups", login: "someone@example.com", groups: []string{"sales@example.com"}, want: false},
		{name: "user listed, groups not", login: "amelie@example.com", groups: []string{"sales@example.com"}, want: true},
		{name: "group named like the listed user", login: "someone@example.com", groups: []string{"amelie@example.com"}, want: false},
		{name: "user named like the listed group", login: "eng@example.com", want: false},
	}
	for _, tt := range tests {
		got, err := db.IsAdmin(tt.login, tt.groups)
		if err != nil {
			t.Errorf("%s: db.IsAdmin(%q, %q) returned error: %v", tt.name, tt.login, tt.groups, err)
			continue
		}
		if got != tt.want {
			t.Errorf("%s: db.IsAdmin(%q, %q) = %v, want %v", tt.name, tt.login, tt.groups, got, tt.want)
		}
	}

	// The table is edited by hand, so it rejects a misspelled group prefix
	// itself rather than silently keeping a row that can never match.
	if _, err := db.db.Exec(`INSERT INTO Admins (Name) VALUES ('groups:eng@example.com')`); err == nil {
		t.Error("inserting a row with a misspelled group prefix succeeded, want CHECK constraint failure")
	}
}

// Test saving, loading, and deleting stats.
func Test_DB_SaveLoadDeleteStats(t *testing.T) {
	db := newTestDB(t)

	// preload some links
	links := []*Link{
		{Short: "a"},
		{Short: "B-c"},
	}
	for _, link := range links {
		if err := db.Save(link); err != nil {
			t.Error(err)
		}
	}

	// Stats to record and then retrieve.
	// Stats to store do not need to be their canonical short name,
	// but returned stats always should be.
	stats := []ClickStats{
		{"a": 1},
		{"b-c": 1},
		{"a": 1, "bc": 2},
	}
	want := ClickStats{
		"a":   2,
		"B-c": 3,
	}

	for _, s := range stats {
		if err := db.SaveStats(s); err != nil {
			t.Error(err)
		}
	}

	got, err := db.LoadStats()
	if err != nil {
		t.Error(err)
	}
	if !cmp.Equal(got, want) {
		t.Errorf("db.LoadStats got %v, want %v", got, want)
	}

	for k := range want {
		if err := db.DeleteStats(k); err != nil {
			t.Error(err)
		}
	}

	got, err = db.LoadStats()
	if err != nil {
		t.Error(err)
	}
	want = ClickStats{}
	if !cmp.Equal(got, want) {
		t.Errorf("db.LoadStats got %v, want %v", got, want)
	}
}

// Test GetLinksByOwner functionality
func Test_DB_GetLinksByOwner(t *testing.T) {
	db := newTestDB(t)

	// preload some links with owner
	links := []*Link{
		{Short: "a", Owner: "foo@bar.com"},
		{Short: "B-c", Owner: "bar@foo.com "},
	}
	for _, link := range links {
		if err := db.Save(link); err != nil {
			t.Error(err)
		}
	}

	want := []*Link{
		{Short: "a", Owner: "foo@bar.com"},
	}
	got, err := db.GetLinksByOwner("foo@bar.com")
	if err != nil {
		t.Error(err)
	}

	if !cmp.Equal(got, want) {
		t.Errorf("db.GetLinksByOwner got %v; want %v", got, want)
	}

	// confirm empty response for non-existant owner
	got, err = db.GetLinksByOwner("foo1@bar.com")
	if err != nil {
		t.Error(err)
	}
	if len(got) != 0 {
		t.Errorf("db.GetLinksByOwner got %v; want empty slice", got)
	}
}

// Test that a search matches the fields it should, and that a query made of
// the characters LIKE reads as wildcards means itself. The escaping is the
// part that differs between the two databases, so this is worth running
// against both.
func Test_DB_SearchLinks(t *testing.T) {
	db := newTestDB(t)

	for _, link := range []*Link{
		{Short: "meeting-notes", Long: "https://docs.example.com/notes"},
		{Short: "hr", Long: "https://app.hibob.com/employees"},
		{Short: "code", Pattern: "https://github.com/search?q={{QueryEscape .Path}}"},
		{Short: "pct", Long: "https://example.com/100%25?x=a_b!c"},
	} {
		if err := db.Save(link); err != nil {
			t.Fatal(err)
		}
	}

	tests := []struct {
		query string
		want  []string
	}{
		{query: "notes", want: []string{"meeting-notes"}},        // the short name
		{query: "MEETING", want: []string{"meeting-notes"}},      // regardless of case
		{query: "meetingnotes", want: []string{"meeting-notes"}}, // the ID, dashes ignored
		{query: "hibob", want: []string{"hr"}},                   // the destination
		{query: "github", want: []string{"code"}},                // the pattern
		{query: "example.com", want: []string{"meeting-notes", "pct"}},
		{query: "%", want: []string{"pct"}}, // a wildcard means itself
		{query: "_", want: []string{"pct"}},
		{query: "!", want: []string{"pct"}}, // including the escape character
		{query: "nothing here", want: nil},
	}
	for _, tt := range tests {
		links, err := db.SearchLinks(tt.query)
		if err != nil {
			t.Errorf("db.SearchLinks(%q) returned error: %v", tt.query, err)
			continue
		}
		var got []string
		for _, link := range links {
			got = append(got, link.Short)
		}
		slices.Sort(got)
		if !cmp.Equal(got, tt.want, cmpopts.EquateEmpty()) {
			t.Errorf("db.SearchLinks(%q) = %v, want %v", tt.query, got, tt.want)
		}
	}
}
