package store

// Map and slice fields are JSON text, times are Unix milliseconds, and a zero
// run id is NULL. The triggers make events append-only at the database level.
var migrations = []Migration{
	{Version: 1, SQL: `
CREATE TABLE projects (
	id              INTEGER PRIMARY KEY AUTOINCREMENT,
	name            TEXT NOT NULL UNIQUE,
	repo_url        TEXT NOT NULL,
	base_branch     TEXT NOT NULL,
	setup_command   TEXT NOT NULL,
	verify_command  TEXT NOT NULL,
	verify_waived   INTEGER NOT NULL,
	tool_overrides  TEXT NOT NULL,
	egress_hosts    TEXT NOT NULL,
	env             TEXT NOT NULL,
	secret_vars     TEXT NOT NULL,
	switches        TEXT NOT NULL,
	baseline_commit TEXT,
	baseline_passed INTEGER,
	baseline_at     INTEGER,
	baseline_output TEXT
);

CREATE TABLE tasks (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	project_id  INTEGER NOT NULL REFERENCES projects(id),
	title       TEXT NOT NULL,
	body        TEXT NOT NULL,
	requester   TEXT NOT NULL,
	base_commit TEXT NOT NULL,
	reference   INTEGER NOT NULL,
	replay_id   INTEGER NOT NULL,
	overrides   TEXT NOT NULL,
	created_at  INTEGER NOT NULL
);

CREATE TABLE runs (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	task_id    INTEGER NOT NULL REFERENCES tasks(id),
	reason     TEXT NOT NULL,
	session_id TEXT NOT NULL,
	outcome    TEXT NOT NULL,
	started_at INTEGER NOT NULL,
	ended_at   INTEGER
);

CREATE TABLE events (
	id      INTEGER PRIMARY KEY AUTOINCREMENT,
	task_id INTEGER NOT NULL REFERENCES tasks(id),
	run_id  INTEGER REFERENCES runs(id),
	at      INTEGER NOT NULL,
	type    TEXT NOT NULL,
	actor   TEXT NOT NULL,
	payload TEXT NOT NULL
);
CREATE INDEX events_task_id ON events (task_id, id);

CREATE TRIGGER events_no_update BEFORE UPDATE ON events
BEGIN SELECT RAISE(ABORT, 'events are append-only'); END;
CREATE TRIGGER events_no_delete BEFORE DELETE ON events
BEGIN SELECT RAISE(ABORT, 'events are append-only'); END;

CREATE TABLE usage (
	id                 INTEGER PRIMARY KEY AUTOINCREMENT,
	task_id            INTEGER NOT NULL REFERENCES tasks(id),
	run_id             INTEGER REFERENCES runs(id),
	event_id           INTEGER REFERENCES events(id),
	source             TEXT NOT NULL,
	model              TEXT NOT NULL,
	input              INTEGER NOT NULL,
	output             INTEGER NOT NULL,
	cache_read         INTEGER NOT NULL,
	cache_write        INTEGER NOT NULL,
	cost_estimate_usd  REAL NOT NULL
);
CREATE INDEX usage_task_id ON usage (task_id);

CREATE TABLE settings (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);

CREATE TABLE secrets (
	name   TEXT PRIMARY KEY,
	sealed BLOB NOT NULL
);
`},
	{Version: 2, SQL: `
CREATE TABLE replays (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	name       TEXT NOT NULL,
	config     TEXT NOT NULL,
	image      TEXT NOT NULL,
	started_at INTEGER NOT NULL,
	ended_at   INTEGER
);

CREATE TABLE replay_items (
	replay_id         INTEGER NOT NULL REFERENCES replays(id),
	source_task_id    INTEGER NOT NULL REFERENCES tasks(id),
	task_id           INTEGER NOT NULL REFERENCES tasks(id),
	verify            TEXT NOT NULL,
	stop              TEXT NOT NULL,
	asked             INTEGER NOT NULL,
	turns             INTEGER NOT NULL,
	tokens            INTEGER NOT NULL,
	cost_estimate_usd REAL NOT NULL,
	duration_ms       INTEGER NOT NULL,
	diff_path         TEXT NOT NULL,
	PRIMARY KEY (replay_id, source_task_id)
);
`},
	{Version: 3, SQL: `
CREATE TABLE task_deps (
	task_id       INTEGER NOT NULL REFERENCES tasks(id),
	depends_on_id INTEGER NOT NULL REFERENCES tasks(id),
	PRIMARY KEY (task_id, depends_on_id)
);

ALTER TABLE projects ADD COLUMN check_requested_at INTEGER;
ALTER TABLE projects ADD COLUMN check_result TEXT NOT NULL DEFAULT '{}';
`},
	{Version: 4, SQL: `
CREATE TABLE users (
	id            INTEGER PRIMARY KEY CHECK (id = 1),
	name          TEXT NOT NULL,
	password_hash TEXT NOT NULL,
	created_at    INTEGER NOT NULL
);

CREATE TABLE sessions (
	token_hash TEXT PRIMARY KEY,
	created_at INTEGER NOT NULL,
	expires_at INTEGER NOT NULL
);

CREATE TABLE api_tokens (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	name         TEXT NOT NULL UNIQUE,
	token_hash   TEXT NOT NULL UNIQUE,
	created_at   INTEGER NOT NULL,
	last_used_at INTEGER
);
`},
	{Version: 5, SQL: `
UPDATE settings SET key = 'task_timeout_minutes' WHERE key = 'work_timeout_minutes';
UPDATE settings SET key = 'task_max_turns' WHERE key = 'work_max_turns';

ALTER TABLE projects ADD COLUMN caps TEXT NOT NULL DEFAULT '{}';
`},
	{Version: 6, SQL: `
ALTER TABLE projects ADD COLUMN forge TEXT NOT NULL DEFAULT '{}';

CREATE INDEX events_at ON events (at);
CREATE INDEX usage_event_id ON usage (event_id);
`},
	{Version: 7, SQL: `
-- Dropping the name's UNIQUE needs a rebuild. Creating the new table and
-- renaming it over the old one failed at commit on tasks.project_id, so the
-- rows go through a holding table and the foreign key check waits for commit.
PRAGMA defer_foreign_keys = ON;
CREATE TABLE projects_old AS SELECT * FROM projects;
DROP TABLE projects;
CREATE TABLE projects (
	id              INTEGER PRIMARY KEY AUTOINCREMENT,
	name            TEXT NOT NULL,
	repo_url        TEXT NOT NULL,
	base_branch     TEXT NOT NULL,
	setup_command   TEXT NOT NULL,
	verify_command  TEXT NOT NULL,
	verify_waived   INTEGER NOT NULL,
	tool_overrides  TEXT NOT NULL,
	egress_hosts    TEXT NOT NULL,
	env             TEXT NOT NULL,
	secret_vars     TEXT NOT NULL,
	switches        TEXT NOT NULL,
	baseline_commit TEXT,
	baseline_passed INTEGER,
	baseline_at     INTEGER,
	baseline_output TEXT,
	check_requested_at INTEGER,
	check_result    TEXT NOT NULL DEFAULT '{}',
	caps            TEXT NOT NULL DEFAULT '{}',
	forge           TEXT NOT NULL DEFAULT '{}',
	removed_at      INTEGER
);
INSERT INTO projects (id, name, repo_url, base_branch, setup_command, verify_command, verify_waived, tool_overrides, egress_hosts, env, secret_vars, switches, baseline_commit, baseline_passed, baseline_at, baseline_output, check_requested_at, check_result, caps, forge)
SELECT id, name, repo_url, base_branch, setup_command, verify_command, verify_waived, tool_overrides, egress_hosts, env, secret_vars, switches, baseline_commit, baseline_passed, baseline_at, baseline_output, check_requested_at, check_result, caps, forge FROM projects_old;
DROP TABLE projects_old;
CREATE UNIQUE INDEX projects_name ON projects (name) WHERE removed_at IS NULL;
`},
	{Version: 8, SQL: `
CREATE TABLE forge_credentials (
	name   TEXT PRIMARY KEY,
	kind   TEXT NOT NULL,
	app_id INTEGER NOT NULL DEFAULT 0
);

ALTER TABLE projects ADD COLUMN forge_credential TEXT REFERENCES forge_credentials (name);
`},
}
