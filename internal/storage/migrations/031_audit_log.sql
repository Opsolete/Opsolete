-- Append-only audit trail: who did what (every state-changing request, sign-ins, denied requests,
-- runs the VCS poller queues, tokens created on the command line). Rows can't be updated, deleted
-- or truncated. Each row carries an HMAC (keyed with SECRET_KEY, which the database never sees)
-- over its content and the previous row's hash, so even someone who can get around the triggers
-- can't rewrite the log without breaking the chain (Store.VerifyAuditLog). Ids have no gaps.
-- No foreign keys: entries outlive the workspaces and users they name.
CREATE TABLE audit_log (
    id        bigint PRIMARY KEY,
    at        timestamptz NOT NULL,
    actor     text NOT NULL,  -- email, "vcs" for the poller, "cli" for `opsolete token`, "anonymous"
    action    text NOT NULL,  -- e.g. "POST /workspaces/prod/runs/<id>/confirm"
    detail    json NOT NULL,  -- json, not jsonb: the exact text the HMAC covers
    prev_hash bytea NOT NULL, -- '' for the first row
    hash      bytea NOT NULL
);

CREATE FUNCTION audit_log_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'audit_log is append-only';
END $$;
CREATE TRIGGER audit_log_no_update BEFORE UPDATE OR DELETE ON audit_log FOR EACH ROW EXECUTE FUNCTION audit_log_immutable();
CREATE TRIGGER audit_log_no_truncate BEFORE TRUNCATE ON audit_log FOR EACH STATEMENT EXECUTE FUNCTION audit_log_immutable();
