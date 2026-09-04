// Copyright 2022 Tailscale Inc & Contributors
// SPDX-License-Identifier: BSD-3-Clause

package golink

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
	"tailscale.com/tstime"
)

// Link is the structure stored for each go short link.
type Link struct {
	Short string // the "foo" part of http://go/foo
	Long  string // where http://go/foo goes, used exactly as written

	// Pattern is the text/template expanded for a path below the link's name,
	// which is available to it as .Path. A link with no pattern answers to its
	// own name and nothing else, leaving the paths below it free to become
	// links of their own; a link with one answers to both.
	//
	// It is always exported, even when empty, so that a link written before
	// patterns existed, which kept its template in Long, can be told apart
	// from one that simply has no pattern. See UnmarshalJSON.
	Pattern string

	Created  time.Time
	LastEdit time.Time // when the link was last edited
	Owner    string    // user@domain
	// Locked reports whether only Owner and admins may edit the link.
	// It is omitted when exporting unlocked links, so that snapshots of
	// databases without any locked links are unchanged.
	Locked bool `json:",omitempty"`
}

// UnmarshalJSON decodes a Link, converting one written before it could have a
// pattern. Such a link kept its template in Long, and answered for the paths
// below its name if it was dynamic; before there was a Dynamic field at all,
// every link did.
func (l *Link) UnmarshalJSON(b []byte) error {
	type link Link // shed the methods of Link, so this does not recurse
	aux := struct {
		*link
		Pattern *string
		Dynamic *bool
	}{link: (*link)(l)}
	if err := json.Unmarshal(b, &aux); err != nil {
		return err
	}
	if aux.Pattern != nil {
		l.Pattern = *aux.Pattern
		return nil
	}
	l.Long, l.Pattern = legacyPattern(l.Long, aux.Dynamic == nil || *aux.Dynamic)
	return nil
}

// legacyPattern splits the destination of a link written before patterns
// existed into a destination and a pattern.
//
// Such a link held either a plain URL or a template in Long, and if it was
// dynamic it also answered for the paths below its name: a template was
// expanded with the remaining path, and a plain URL had that path appended.
func legacyPattern(long string, dynamic bool) (newLong, pattern string) {
	if !dynamic {
		return long, ""
	}
	if strings.Contains(long, "{{") {
		// The template was the whole destination, and was expanded even for
		// the bare name, with an empty path. A link with a pattern and no
		// destination still does exactly that.
		return "", long
	}
	if strings.HasSuffix(long, "/") {
		return long, long + "{{.Path}}"
	}
	return long, long + "/{{.Path}}"
}

// ClickStats is the number of clicks a set of links have received in a given
// time period. It is keyed by link short name, with values of total clicks.
type ClickStats map[string]int

// linkID returns the normalized ID for a link short name.
func linkID(short string) string {
	id := url.PathEscape(strings.ToLower(short))
	id = strings.ReplaceAll(id, "-", "")
	return id
}

// SQLiteDB stores Links in a SQLite database.
type SQLiteDB struct {
	db *sql.DB
	mu sync.RWMutex

	clock tstime.Clock // allow overriding time for tests
}

//go:embed schema.sql
var sqlSchema string

// NewSQLiteDB returns a new SQLiteDB that stores links in a SQLite database stored at f.
func NewSQLiteDB(f string) (*SQLiteDB, error) {
	db, err := sql.Open("sqlite", f)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		return nil, err
	}

	if _, err = db.Exec(sqlSchema); err != nil {
		return nil, err
	}

	return &SQLiteDB{db: db}, nil
}

// Now returns the current time.
func (s *SQLiteDB) Now() time.Time {
	return tstime.DefaultClock{Clock: s.clock}.Now()
}

// linkColumns are the columns of the Links table that make up a Link, in the
// order scanLink reads them.
const linkColumns = "Short, Long, Pattern, Created, LastEdit, Owner, Locked"

// scanLink reads a single Link from a query result.
func scanLink(row interface{ Scan(...any) error }) (*Link, error) {
	link := new(Link)
	var created, lastEdit int64
	if err := row.Scan(&link.Short, &link.Long, &link.Pattern, &created, &lastEdit, &link.Owner, &link.Locked); err != nil {
		return nil, err
	}
	link.Created = time.Unix(created, 0).UTC()
	link.LastEdit = time.Unix(lastEdit, 0).UTC()
	return link, nil
}

// scanLinks reads every Link from a query result and closes it.
func scanLinks(rows *sql.Rows) ([]*Link, error) {
	defer rows.Close()

	var links []*Link
	for rows.Next() {
		link, err := scanLink(rows)
		if err != nil {
			return nil, err
		}
		links = append(links, link)
	}
	return links, rows.Err()
}

// LoadAll returns all stored Links.
//
// The caller owns the returned values.
func (s *SQLiteDB) LoadAll() ([]*Link, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.Query("SELECT " + linkColumns + " FROM Links")
	if err != nil {
		return nil, err
	}
	return scanLinks(rows)
}

// Load returns a Link by its short name.
//
// It returns fs.ErrNotExist if the link does not exist.
//
// The caller owns the returned value.
func (s *SQLiteDB) Load(short string) (*Link, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	row := s.db.QueryRow("SELECT "+linkColumns+" FROM Links WHERE ID = ?1 LIMIT 1", linkID(short))
	link, err := scanLink(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			err = fs.ErrNotExist
		}
		return nil, err
	}
	return link, nil
}

// Save saves a Link.
func (s *SQLiteDB) Save(link *Link) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	result, err := s.db.Exec("INSERT OR REPLACE INTO Links (ID, Short, Long, Pattern, Created, LastEdit, Owner, Locked) VALUES (?, ?, ?, ?, ?, ?, ?, ?)", linkID(link.Short), link.Short, link.Long, link.Pattern, link.Created.Unix(), link.LastEdit.Unix(), link.Owner, boolToInt(link.Locked))
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return fmt.Errorf("expected to affect 1 row, affected %d", rows)
	}
	return nil
}

// boolToInt returns the integer SQLite stores for a boolean column.
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// Delete removes a Link using its short name.
func (s *SQLiteDB) Delete(short string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	result, err := s.db.Exec("DELETE FROM Links WHERE ID = ?", linkID(short))
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return fmt.Errorf("expected to affect 1 row, affected %d", rows)
	}
	return nil
}

// adminGroupPrefix marks a row of the Admins table as naming a group rather
// than a login, in the style of the "group:" prefix used in tailnet ACLs.
const adminGroupPrefix = "group:"

// IsAdmin returns whether the specified login, or any of the groups that login
// belongs to, is listed in the Admins table.
//
// The table is managed by the operator, directly in the database; golink never
// writes to it. It is empty by default, in which case admin rights come only
// from the tailnet ACL grant, as they always have.
func (s *SQLiteDB) IsAdmin(login string, groups []string) (bool, error) {
	// The names that would make this user an admin, if any of them is listed.
	var names []any
	if login != "" {
		names = append(names, strings.ToLower(login))
	}
	for _, group := range groups {
		if group != "" {
			names = append(names, adminGroupPrefix+strings.ToLower(group))
		}
	}
	if len(names) == 0 {
		return false, nil
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	var count int
	query := "SELECT count(*) FROM Admins WHERE LOWER(Name) IN (?" + strings.Repeat(", ?", len(names)-1) + ")"
	if err := s.db.QueryRow(query, names...).Scan(&count); err != nil {
		return false, err
	}
	return count > 0, nil
}

// LoadStats returns click stats for links.
func (s *SQLiteDB) LoadStats() (ClickStats, error) {
	allLinks, err := s.LoadAll()
	if err != nil {
		return nil, err
	}
	linkmap := make(map[string]string, len(allLinks)) // map ID => Short
	for _, link := range allLinks {
		linkmap[linkID(link.Short)] = link.Short
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.Query("SELECT ID, sum(Clicks) FROM Stats GROUP BY ID")
	if err != nil {
		return nil, err
	}
	stats := make(map[string]int)
	for rows.Next() {
		var id string
		var clicks int
		err := rows.Scan(&id, &clicks)
		if err != nil {
			return nil, err
		}
		short := linkmap[id]
		stats[short] = clicks
	}
	return stats, rows.Err()
}

// SaveStats records click stats for links.  The provided map includes
// incremental clicks that have occurred since the last time SaveStats
// was called.
func (s *SQLiteDB) SaveStats(stats ClickStats) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(context.TODO(), nil)
	if err != nil {
		return err
	}
	now := s.Now().Unix()
	for short, clicks := range stats {
		_, err := tx.Exec("INSERT INTO Stats (ID, Created, Clicks) VALUES (?, ?, ?)", linkID(short), now, clicks)
		if err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// DeleteStats deletes click stats for a link.
func (s *SQLiteDB) DeleteStats(short string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec("DELETE FROM Stats WHERE ID = ?", linkID(short))
	if err != nil {
		return err
	}
	return nil
}

// GetLinksByOwner returns all Links owned by the specified owner.
func (s *SQLiteDB) GetLinksByOwner(owner string) ([]*Link, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.Query("SELECT "+linkColumns+" FROM Links WHERE LOWER(Owner) = LOWER(?)", owner)
	if err != nil {
		return nil, err
	}
	return scanLinks(rows)
}

// SearchLinks returns all Links whose short name, destination or pattern
// contains query, matched without regard to case. Dashes are ignored in the
// short name, as they are when resolving a link, so that "meetingnotes" finds
// "meeting-notes".
func (s *SQLiteDB) SearchLinks(query string) ([]*Link, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.Query(`SELECT `+linkColumns+` FROM Links WHERE
		Short LIKE ?1 ESCAPE '\' OR
		Long LIKE ?1 ESCAPE '\' OR
		Pattern LIKE ?1 ESCAPE '\' OR
		ID LIKE ?2 ESCAPE '\'`,
		containsPattern(query), containsPattern(linkID(query)))
	if err != nil {
		return nil, err
	}
	return scanLinks(rows)
}

// containsPattern returns a SQL LIKE pattern matching any string that contains
// s, with the wildcards LIKE would otherwise read in s escaped.
func containsPattern(s string) string {
	var b strings.Builder
	b.WriteByte('%')
	for _, r := range s {
		if r == '\\' || r == '%' || r == '_' {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	b.WriteByte('%')
	return b.String()
}
