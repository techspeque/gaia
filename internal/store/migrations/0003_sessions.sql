-- Sessions (OVERVIEW §2.2). Times are Unix nanoseconds, UTC. argv is a JSON
-- array of strings. profile is NULL when the session names none; repo_remote,
-- branch and slice are '' when none. There is no column for an environment,
-- a token or a key: a session row never holds a credential.
--
-- node_id has no foreign key: a session's record outlives its node's
-- registration, and removing a node must not fail on its history.
CREATE TABLE sessions (
    id          TEXT    NOT NULL PRIMARY KEY,
    node_id     TEXT    NOT NULL,
    profile     TEXT,
    repo_remote TEXT    NOT NULL DEFAULT '',
    branch      TEXT    NOT NULL DEFAULT '',
    slice       TEXT    NOT NULL DEFAULT '',
    mode        TEXT    NOT NULL CHECK (mode IN ('headless', 'interactive')),
    argv        TEXT    NOT NULL CHECK (json_valid(argv) AND json_type(argv) = 'array' AND json_array_length(argv) > 0),
    state       TEXT    NOT NULL CHECK (state IN ('starting', 'running', 'lost', 'exited', 'stopped', 'failed')),
    started_at  INTEGER NOT NULL,
    ended_at    INTEGER,
    exit_code   INTEGER,
    -- A terminal state has ended_at; a live one has neither ended_at nor an
    -- exit code. An agent that exited by itself has an exit code.
    CHECK ((state IN ('exited', 'stopped', 'failed')) = (ended_at IS NOT NULL)),
    CHECK (exit_code IS NULL OR ended_at IS NOT NULL),
    CHECK (state <> 'exited' OR exit_code IS NOT NULL)
) STRICT;

CREATE INDEX sessions_node_id ON sessions (node_id);

-- A session in a terminal state never changes again, and no session row is
-- deleted, whatever SQL is run.
CREATE TRIGGER sessions_terminal_frozen
BEFORE UPDATE ON sessions
WHEN OLD.state IN ('exited', 'stopped', 'failed')
BEGIN
    SELECT RAISE(ABORT, 'session is in a terminal state');
END;

CREATE TRIGGER sessions_no_delete
BEFORE DELETE ON sessions
BEGIN
    SELECT RAISE(ABORT, 'sessions are not deleted');
END;
