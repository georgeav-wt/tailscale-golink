// Copyright 2022 Tailscale Inc & Contributors
// SPDX-License-Identifier: BSD-3-Clause

package golink

import (
	"path"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

// Test saving, loading, and deleting links for SQLiteDB.
func Test_SQLiteDB_SaveLoadDeleteLinks(t *testing.T) {
	db, err := NewSQLiteDB(path.Join(t.TempDir(), "links.db"))
	if err != nil {
		t.Error(err)
	}

	links := []*Link{
		{Short: "short", Long: "long"},
		{Short: "Foo.Bar", Long: "long"},
		{Short: "locked", Long: "long", Owner: "a@example.com", Locked: true},
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
func Test_SQLiteDB_IsAdmin(t *testing.T) {
	db, err := NewSQLiteDB(path.Join(t.TempDir(), "links.db"))
	if err != nil {
		t.Fatal(err)
	}

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

// Test saving, loading, and deleting stats for SQLiteDB.
func Test_SQLiteDB_SaveLoadDeleteStats(t *testing.T) {
	db, err := NewSQLiteDB(path.Join(t.TempDir(), "links.db"))
	if err != nil {
		t.Error(err)
	}

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
func Test_SQLiteDB_GetLinksByOwner(t *testing.T) {
	db, err := NewSQLiteDB(path.Join(t.TempDir(), "links.db"))
	if err != nil {
		t.Error(err)
	}

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
