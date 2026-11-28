-- A session's after-session push (ADR-0010 rule 3, launcher contract 94):
-- '' for a session in no checkout, 'pending' until the node has pushed,
-- then 'done' or 'failed'. The push ends after the session does, so sync is
-- the one column a terminal row may still change, and only from '' or
-- 'pending' to a later state; a settled sync never changes.
ALTER TABLE sessions ADD COLUMN sync TEXT NOT NULL DEFAULT ''
    CHECK (sync IN ('', 'pending', 'done', 'failed'));

DROP TRIGGER sessions_terminal_frozen;

CREATE TRIGGER sessions_terminal_frozen
BEFORE UPDATE ON sessions
WHEN OLD.state IN ('exited', 'stopped', 'failed')
 AND NOT (NEW.sync IS NOT OLD.sync AND NEW.id IS OLD.id AND NEW.node_id IS OLD.node_id AND NEW.profile IS OLD.profile
      AND NEW.repo_remote IS OLD.repo_remote AND NEW.branch IS OLD.branch AND NEW.slice IS OLD.slice
      AND NEW.mode IS OLD.mode AND NEW.argv IS OLD.argv AND NEW.state IS OLD.state
      AND NEW.started_at IS OLD.started_at AND NEW.ended_at IS OLD.ended_at
      AND NEW.exit_code IS OLD.exit_code)
BEGIN
    SELECT RAISE(ABORT, 'session is in a terminal state');
END;

CREATE TRIGGER sessions_sync_forward
BEFORE UPDATE OF sync ON sessions
WHEN NEW.sync IS NOT OLD.sync
 AND NOT (OLD.sync = '' OR (OLD.sync = 'pending' AND NEW.sync IN ('done', 'failed')))
BEGIN
    SELECT RAISE(ABORT, 'session sync cannot move back');
END;
