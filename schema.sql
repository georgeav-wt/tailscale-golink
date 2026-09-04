CREATE TABLE IF NOT EXISTS Links (
	ID       TEXT    PRIMARY KEY,         -- normalized version of Short (foobar)
	Short    TEXT    NOT NULL DEFAULT "", -- user-provided Short name (Foo-Bar)
	Long     TEXT    NOT NULL DEFAULT "",
	Created  INTEGER NOT NULL DEFAULT (strftime('%s', 'now')), -- unix seconds
	LastEdit INTEGER NOT NULL DEFAULT (strftime('%s', 'now')), -- unix seconds
	Owner	 TEXT    NOT NULL DEFAULT "",
	LastEditBy TEXT  NOT NULL DEFAULT "", -- who saved it last, if known
	Locked   INTEGER NOT NULL DEFAULT 0,   -- if 1, only Owner or an admin may edit
	-- the template expanded for a path below the link's name. A link with no
	-- pattern answers to its own name only.
	Pattern  TEXT    NOT NULL DEFAULT ""
);

-- Admins is managed by the operator, directly in the database. golink only ever
-- reads it; it grants admin rights in addition to any tailnet ACL grant.
CREATE TABLE IF NOT EXISTS Admins (
	-- a login, or "group:" followed by a group the user belongs to. NOCASE so
	-- that the same name in a different case is the same row, however it was
	-- typed in. A name containing a colon has to be a group, which catches a
	-- misspelled prefix that would otherwise sit here matching nobody.
	Name     TEXT    PRIMARY KEY COLLATE NOCASE
	                 CHECK (Name NOT LIKE '%:%' OR Name LIKE 'group:%'),
	Created  INTEGER NOT NULL DEFAULT (strftime('%s', 'now')) -- unix seconds
);

CREATE TABLE IF NOT EXISTS Stats (
	ID       TEXT    NOT NULL DEFAULT "",
	Created  INTEGER NOT NULL DEFAULT (strftime('%s', 'now')), -- unix seconds
	Clicks   INTEGER
);
