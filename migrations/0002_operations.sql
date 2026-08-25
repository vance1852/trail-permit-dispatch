-- 0002_operations: parties, registered members, progress reports, incidents
-- and permit fee settlements.

CREATE TABLE parties (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    code             TEXT    NOT NULL,
    trail_id         INTEGER NOT NULL REFERENCES trails (id) ON DELETE RESTRICT,
    leader_id        INTEGER NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
    permit_window_id INTEGER NULL REFERENCES permit_windows (id) ON DELETE RESTRICT,
    hike_day         TEXT    NOT NULL,
    planned_start    INTEGER NOT NULL,
    planned_end      INTEGER NOT NULL,
    size             INTEGER NOT NULL CHECK (size > 0),
    state            TEXT    NOT NULL CHECK (state IN ('draft', 'permit_reserved', 'confirmed', 'on_trail', 'completed', 'cancelled', 'aborted')),
    version          INTEGER NOT NULL DEFAULT 1,
    contact_phone    TEXT    NOT NULL DEFAULT '',
    notes            TEXT    NOT NULL DEFAULT '',
    confirmed_at     INTEGER NULL,
    dispatched_at    INTEGER NULL,
    closed_at        INTEGER NULL,
    created_at       INTEGER NOT NULL,
    updated_at       INTEGER NOT NULL
);

CREATE UNIQUE INDEX ux_parties_code ON parties (code);
CREATE INDEX ix_parties_leader_state ON parties (leader_id, state);
CREATE INDEX ix_parties_trail_day ON parties (trail_id, hike_day);
CREATE INDEX ix_parties_state_day ON parties (state, hike_day);

CREATE TABLE party_members (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    party_id      INTEGER NOT NULL REFERENCES parties (id) ON DELETE CASCADE,
    member_ref    TEXT    NOT NULL,
    display_name  TEXT    NOT NULL,
    kind          TEXT    NOT NULL CHECK (kind IN ('leader', 'companion')),
    waiver_signed INTEGER NOT NULL DEFAULT 0 CHECK (waiver_signed IN (0, 1)),
    phone         TEXT    NOT NULL,
    joined_at     INTEGER NOT NULL
);

CREATE UNIQUE INDEX ux_party_members_ref ON party_members (party_id, member_ref);

CREATE TABLE checkpoint_reports (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    party_id      INTEGER NOT NULL REFERENCES parties (id) ON DELETE CASCADE,
    checkpoint_id INTEGER NOT NULL REFERENCES checkpoints (id) ON DELETE RESTRICT,
    seq           INTEGER NOT NULL,
    status        TEXT    NOT NULL CHECK (status IN ('on_time', 'late', 'missed')),
    head_count    INTEGER NOT NULL CHECK (head_count >= 0),
    note          TEXT    NOT NULL DEFAULT '',
    reported_at   INTEGER NOT NULL,
    reported_by   INTEGER NOT NULL REFERENCES users (id) ON DELETE RESTRICT
);

CREATE UNIQUE INDEX ux_checkpoint_reports_party_checkpoint ON checkpoint_reports (party_id, checkpoint_id);
CREATE INDEX ix_checkpoint_reports_party_seq ON checkpoint_reports (party_id, seq);

CREATE TABLE incidents (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    party_id         INTEGER NOT NULL REFERENCES parties (id) ON DELETE CASCADE,
    kind             TEXT    NOT NULL,
    severity         TEXT    NOT NULL CHECK (severity IN ('minor', 'major', 'critical')),
    state            TEXT    NOT NULL CHECK (state IN ('open', 'escalated', 'resolved', 'closed')),
    summary          TEXT    NOT NULL,
    checkpoint_seq   INTEGER NOT NULL DEFAULT 0,
    escalation_count INTEGER NOT NULL DEFAULT 0 CHECK (escalation_count >= 0),
    -- opened_by is 0 when the platform itself opened the incident, for example
    -- the overdue checkpoint sweep; otherwise it is the reporting account.
    opened_by        INTEGER NOT NULL DEFAULT 0 CHECK (opened_by >= 0),
    opened_at        INTEGER NOT NULL,
    resolved_at      INTEGER NULL,
    resolution       TEXT    NOT NULL DEFAULT '',
    version          INTEGER NOT NULL DEFAULT 1,
    updated_at       INTEGER NOT NULL
);

CREATE INDEX ix_incidents_party_state ON incidents (party_id, state);
CREATE INDEX ix_incidents_state_severity ON incidents (state, severity);
CREATE UNIQUE INDEX ux_incidents_party_kind_open ON incidents (party_id, kind, checkpoint_seq)
    WHERE state IN ('open', 'escalated');

CREATE TABLE settlements (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    party_id       INTEGER NOT NULL REFERENCES parties (id) ON DELETE CASCADE,
    seats          INTEGER NOT NULL CHECK (seats > 0),
    unit_fee_cents INTEGER NOT NULL CHECK (unit_fee_cents >= 0),
    total_cents    INTEGER NOT NULL CHECK (total_cents >= 0),
    state          TEXT    NOT NULL CHECK (state IN ('pending', 'settled', 'waived', 'failed')),
    reference      TEXT    NOT NULL DEFAULT '',
    failure_note   TEXT    NOT NULL DEFAULT '',
    settled_at     INTEGER NULL,
    version        INTEGER NOT NULL DEFAULT 1,
    created_at     INTEGER NOT NULL,
    updated_at     INTEGER NOT NULL
);

CREATE UNIQUE INDEX ux_settlements_party ON settlements (party_id);
CREATE INDEX ix_settlements_state ON settlements (state, created_at);
