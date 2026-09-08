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

	"github.com/go-sql-driver/mysql"
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
	// LastEditBy is who last saved the link. It is empty for a link last
	// saved before golink recorded this, which cannot be worked out after
	// the fact.
	LastEditBy string `json:",omitempty"`
	Owner      string // user@domain
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

// DB stores Links in a SQL database, either SQLite or MySQL.
//
// Which of the two it is makes almost no difference past the connection: every
// statement below is written so that both accept it, which is cheaper to keep
// right than two sets of queries would be. The exceptions live in dialect.
type DB struct {
	db      *sql.DB
	dialect dialect
	mu      sync.RWMutex

	clock tstime.Clock // allow overriding time for tests
}

// dialect is the little that differs between the two databases.
type dialect struct {
	// name is what to call this database in a message.
	name string
	// schema is the DDL that brings an empty database up to date. It is
	// applied on every start, and creates only what is missing; it does not
	// alter a table that already exists. See CLAUDE.md.
	schema string
	// countsReplaceTwice says the database reports a REPLACE that replaced an
	// existing row as having affected two rows, counting the delete and the
	// insert separately, as MySQL does.
	countsReplaceTwice bool
}

//go:embed schema.sql
var sqlSchema string

//go:embed schema-mysql.sql
var mysqlSchema string

// replacedOneRow reports whether a REPLACE meant to write a single row did.
func (d dialect) replacedOneRow(rows int64) bool {
	return rows == 1 || (d.countsReplaceTwice && rows == 2)
}

// NewSQLiteDB returns a new DB that stores links in a SQLite database stored at f.
func NewSQLiteDB(f string) (*DB, error) {
	db, err := sql.Open("sqlite", f)
	if err != nil {
		return nil, err
	}
	return newDB(db, dialect{name: "SQLite", schema: sqlSchema})
}

// NewMySQLDB returns a new DB that stores links in the MySQL database the DSN
// names, in the form user:password@tcp(host:3306)/golink.
//
// Unlike a SQLite file, one MySQL database can back more than one instance of
// golink; see CLAUDE.md for what else that takes.
func NewMySQLDB(dsn string) (*DB, error) {
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		// The driver's own parse errors say what is wrong without quoting the
		// DSN, which holds a password. Checked, and worth re-checking if this
		// ever wraps something else.
		return nil, err
	}
	// A query carries no deadline of its own, so a database that has stopped
	// answering -- rather than refusing, which fails at once -- would wedge
	// every request behind it until something restarted golink. These are
	// defaults: a DSN that sets them keeps its own values.
	if cfg.Timeout == 0 {
		cfg.Timeout = 5 * time.Second
	}
	if cfg.ReadTimeout == 0 {
		cfg.ReadTimeout = 10 * time.Second
	}
	if cfg.WriteTimeout == 0 {
		cfg.WriteTimeout = 10 * time.Second
	}
	connector, err := mysql.NewConnector(cfg)
	if err != nil {
		return nil, err
	}
	db := sql.OpenDB(connector)
	// A handful of connections is plenty for a link shortener, and retiring
	// them keeps golink from holding one that a proxy or a failover has
	// silently taken away underneath it.
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(3 * time.Minute)
	// Shorter than any wait_timeout or idle middlebox is likely to be, because
	// a connection closed at the far end is handed out again and fails the
	// query rather than being retried: this driver's "invalid connection" is
	// not the error database/sql retries on. Observed once, during a flush.
	db.SetConnMaxIdleTime(time.Minute)
	return newDB(db, dialect{name: "MySQL", schema: mysqlSchema, countsReplaceTwice: true})
}

// newDB connects, applies the schema and returns the DB.
func newDB(sqlDB *sql.DB, d dialect) (*DB, error) {
	if err := sqlDB.Ping(); err != nil {
		return nil, err
	}
	if err := execSchema(sqlDB, d); err != nil {
		return nil, err
	}
	return &DB{db: sqlDB, dialect: d}, nil
}

// execSchema applies a schema one statement at a time.
//
// Handing several statements to a single Exec works in SQLite but not in
// MySQL, whose driver refuses them unless the DSN carries multiStatements,
// which is a setting that widens what any SQL injection could reach. Splitting
// them here keeps that out of the DSN.
//
// Comments are removed before the split, so a semicolon in one of them is
// harmless; a schema file may not contain a semicolon inside a string literal,
// and neither of ours does.
func execSchema(db *sql.DB, d dialect) error {
	for i, stmt := range strings.Split(withoutSQLComments(d.schema), ";") {
		if strings.TrimSpace(stmt) == "" {
			continue // the whitespace after the last statement
		}
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("%s schema statement %d: %w", d.name, i+1, err)
		}
	}
	return nil
}

// withoutSQLComments returns s with its -- comments removed, so that neither a
// semicolon nor a statement can be hiding in one.
func withoutSQLComments(s string) string {
	var b strings.Builder
	for line := range strings.SplitSeq(s, "\n") {
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// Ping reports whether the database is reachable, reconnecting if it can. It
// is what a healthcheck asks, so it must be cheap: no query, just a connection.
func (s *DB) Ping() error {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.db.Ping()
}

// Now returns the current time.
func (s *DB) Now() time.Time {
	return tstime.DefaultClock{Clock: s.clock}.Now()
}

// linkColumns are the columns of the Links table that make up a Link, in the
// order scanLink reads them.
//
// Long is quoted because LONG is a reserved word in MySQL, which rejects it as
// a bare identifier anywhere, DDL and queries alike. SQLite reads a backtick
// as a quote too, for exactly this compatibility.
const linkColumns = "Short, `Long`, Pattern, Created, LastEdit, LastEditBy, Owner, Locked"

// scanLink reads a single Link from a query result.
func scanLink(row interface{ Scan(...any) error }) (*Link, error) {
	link := new(Link)
	var created, lastEdit int64
	if err := row.Scan(&link.Short, &link.Long, &link.Pattern, &created, &lastEdit, &link.LastEditBy, &link.Owner, &link.Locked); err != nil {
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
func (s *DB) LoadAll() ([]*Link, error) {
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
func (s *DB) Load(short string) (*Link, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	row := s.db.QueryRow("SELECT "+linkColumns+" FROM Links WHERE ID = ? LIMIT 1", linkID(short))
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
func (s *DB) Save(link *Link) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// REPLACE rather than INSERT OR REPLACE: the short form is the one both
	// databases accept, and it means the same thing in each.
	result, err := s.db.Exec("REPLACE INTO Links (ID, Short, `Long`, Pattern, Created, LastEdit, LastEditBy, Owner, Locked) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)", linkID(link.Short), link.Short, link.Long, link.Pattern, link.Created.Unix(), link.LastEdit.Unix(), link.LastEditBy, link.Owner, boolToInt(link.Locked))
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if !s.dialect.replacedOneRow(rows) {
		return fmt.Errorf("expected to affect 1 row, affected %d", rows)
	}
	return nil
}

// boolToInt returns the integer a boolean column is stored as.
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// Delete removes a Link using its short name.
func (s *DB) Delete(short string) error {
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
func (s *DB) IsAdmin(login string, groups []string) (bool, error) {
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
func (s *DB) LoadStats() (ClickStats, error) {
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
func (s *DB) SaveStats(stats ClickStats) error {
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
func (s *DB) DeleteStats(short string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec("DELETE FROM Stats WHERE ID = ?", linkID(short))
	if err != nil {
		return err
	}
	return nil
}

// GetLinksByOwner returns all Links owned by the specified owner.
func (s *DB) GetLinksByOwner(owner string) ([]*Link, error) {
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
func (s *DB) SearchLinks(query string) ([]*Link, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// The pattern is repeated rather than numbered, and escaped with ! rather
	// than a backslash, because MySQL accepts neither ?1 nor a lone backslash
	// in a string literal. Both are read the same way by SQLite.
	contains, containsID := containsPattern(query), containsPattern(linkID(query))
	rows, err := s.db.Query("SELECT "+linkColumns+" FROM Links WHERE "+
		"Short LIKE ? ESCAPE '!' OR "+
		"`Long` LIKE ? ESCAPE '!' OR "+
		"Pattern LIKE ? ESCAPE '!' OR "+
		"ID LIKE ? ESCAPE '!'",
		contains, contains, contains, containsID)
	if err != nil {
		return nil, err
	}
	return scanLinks(rows)
}

// containsPattern returns a SQL LIKE pattern matching any string that contains
// s, with the wildcards LIKE would otherwise read in s escaped. The escape
// character is ! rather than a backslash, which MySQL reads inside a string
// literal before LIKE ever sees it; the queries say ESCAPE '!' to match.
func containsPattern(s string) string {
	var b strings.Builder
	b.WriteByte('%')
	for _, r := range s {
		if r == '!' || r == '%' || r == '_' {
			b.WriteByte('!')
		}
		b.WriteRune(r)
	}
	b.WriteByte('%')
	return b.String()
}

// LinkCount returns how many links are stored, for the metric of that name.
func (s *DB) LinkCount() (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var count int
	err := s.db.QueryRow("SELECT COUNT(DISTINCT ID) FROM Links").Scan(&count)
	return count, err
}

// StatRows calls f for each row of the Stats ledger, oldest first. Each row is
// the clicks one link received in the minute Created names, so a link has as
// many rows as it has had minutes with a click in them.
func (s *DB) StatRows(f func(id string, created int64, clicks int) error) error {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.Query("SELECT ID, Created, Clicks FROM Stats ORDER BY Created, ID")
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var id string
		var created int64
		var clicks int
		if err := rows.Scan(&id, &created, &clicks); err != nil {
			return err
		}
		if err := f(id, created, clicks); err != nil {
			return err
		}
	}
	return rows.Err()
}
