-- The MySQL version of schema.sql, which it has to be kept in step with. The
-- two differ only in their DDL: every statement golink runs is written to be
-- read the same way by both databases. See db.go.
--
-- Expression defaults need MySQL 8.0.13 or newer. TEXT columns may not have a
-- default at all, so Long and Pattern have none and a row inserted by hand has
-- to give them; golink always does.
--
-- execSchema strips these comments and splits what is left on the semicolons,
-- so a statement may not have one inside a string literal.

CREATE TABLE IF NOT EXISTS Links (
	ID       VARCHAR(255) PRIMARY KEY,         -- normalized version of Short (foobar)
	Short    VARCHAR(255) NOT NULL DEFAULT '', -- user-provided Short name (Foo-Bar)
	`Long`   TEXT         NOT NULL, -- quoted: LONG is a reserved word in MySQL
	Created  BIGINT       NOT NULL DEFAULT (UNIX_TIMESTAMP()), -- unix seconds
	LastEdit BIGINT       NOT NULL DEFAULT (UNIX_TIMESTAMP()), -- unix seconds
	Owner    VARCHAR(255) NOT NULL DEFAULT '',
	LastEditBy VARCHAR(255) NOT NULL DEFAULT '', -- who saved it last, if known
	Locked   TINYINT(1)   NOT NULL DEFAULT 0,  -- if 1, only Owner or an admin may edit
	-- the template expanded for a path below the link's name. A link with no
	-- pattern answers to its own name only.
	Pattern  TEXT         NOT NULL
) CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci;

-- Admins is managed by the operator, directly in the database. golink only ever
-- reads it; it grants admin rights in addition to any tailnet ACL grant.
CREATE TABLE IF NOT EXISTS Admins (
	-- a login, or "group:" followed by a group the user belongs to. The
	-- table's collation is case-insensitive, as SQLite's COLLATE NOCASE is, so
	-- that the same name in a different case is the same row however it was
	-- typed in. A name containing a colon has to be a group, which catches a
	-- misspelled prefix that would otherwise sit here matching nobody.
	Name     VARCHAR(255) PRIMARY KEY,
	Created  BIGINT       NOT NULL DEFAULT (UNIX_TIMESTAMP()), -- unix seconds
	CONSTRAINT AdminIsUserOrGroup CHECK (Name NOT LIKE '%:%' OR Name LIKE 'group:%')
) CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci;

CREATE TABLE IF NOT EXISTS Stats (
	ID       VARCHAR(255) NOT NULL DEFAULT '',
	Created  BIGINT       NOT NULL DEFAULT (UNIX_TIMESTAMP()), -- unix seconds
	Clicks   INT
) CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci;
