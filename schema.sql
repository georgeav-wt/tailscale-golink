CREATE TABLE IF NOT EXISTS Links (
	ID       TEXT    PRIMARY KEY,         -- normalized version of Short (foobar)
	Short    TEXT    NOT NULL DEFAULT "", -- user-provided Short name (Foo-Bar)
	Long     TEXT    NOT NULL DEFAULT "",
	Created  INTEGER NOT NULL DEFAULT (strftime('%s', 'now')), -- unix seconds
	LastEdit INTEGER NOT NULL DEFAULT (strftime('%s', 'now')), -- unix seconds
	Owner	 TEXT    NOT NULL DEFAULT "",
	Locked   INTEGER NOT NULL DEFAULT 0    -- if 1, only Owner or an admin may edit
);

CREATE TABLE IF NOT EXISTS Stats (
	ID       TEXT    NOT NULL DEFAULT "",
	Created  INTEGER NOT NULL DEFAULT (strftime('%s', 'now')), -- unix seconds
	Clicks   INTEGER
);
