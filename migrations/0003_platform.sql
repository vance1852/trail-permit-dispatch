-- 0003_platform: audit trail, background job queue and idempotency records.

CREATE TABLE audit_events (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    request_id  TEXT    NOT NULL DEFAULT '',
    actor_id    INTEGER NOT NULL DEFAULT 0,
    actor_role  TEXT    NOT NULL DEFAULT '',
    action      TEXT    NOT NULL,
    object_type TEXT    NOT NULL,
    object_id   TEXT    NOT NULL,
    result      TEXT    NOT NULL CHECK (result IN ('success', 'failure')),
    detail      TEXT    NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL
);

CREATE INDEX ix_audit_events_object ON audit_events (object_type, object_id, id);
CREATE INDEX ix_audit_events_actor ON audit_events (actor_id, id);
CREATE INDEX ix_audit_events_request ON audit_events (request_id);

CREATE TABLE jobs (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    kind         TEXT    NOT NULL CHECK (kind IN ('notify_dispatch', 'settle_party', 'escalate_incident', 'sweep_overdue_checkpoints', 'expire_sessions')),
    payload      TEXT    NOT NULL DEFAULT '{}',
    state        TEXT    NOT NULL CHECK (state IN ('queued', 'running', 'done', 'failed')),
    attempts     INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    max_attempts INTEGER NOT NULL CHECK (max_attempts > 0),
    run_at       INTEGER NOT NULL,
    locked_by    TEXT    NOT NULL DEFAULT '',
    locked_until INTEGER NULL,
    last_error   TEXT    NOT NULL DEFAULT '',
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL
);

CREATE INDEX ix_jobs_due ON jobs (state, run_at, id);
CREATE INDEX ix_jobs_kind_state ON jobs (kind, state);

CREATE TABLE idempotency_records (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    scope         TEXT    NOT NULL,
    idem_key      TEXT    NOT NULL,
    actor_id      INTEGER NOT NULL,
    request_hash  TEXT    NOT NULL,
    response_body TEXT    NOT NULL DEFAULT '',
    created_at    INTEGER NOT NULL,
    expires_at    INTEGER NOT NULL
);

CREATE UNIQUE INDEX ux_idempotency_scope_key ON idempotency_records (scope, idem_key, actor_id);
CREATE INDEX ix_idempotency_expiry ON idempotency_records (expires_at);
