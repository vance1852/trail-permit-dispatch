-- 0001_core: accounts, revocable sessions, governed trails and permit windows.

CREATE TABLE users (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    email          TEXT    NOT NULL,
    display_name   TEXT    NOT NULL,
    role           TEXT    NOT NULL CHECK (role IN ('leader', 'ranger')),
    status         TEXT    NOT NULL CHECK (status IN ('active', 'suspended')),
    password_hash  TEXT    NOT NULL,
    password_salt  TEXT    NOT NULL,
    kdf_iterations INTEGER NOT NULL CHECK (kdf_iterations > 0),
    created_at     INTEGER NOT NULL,
    updated_at     INTEGER NOT NULL
);

CREATE UNIQUE INDEX ux_users_email ON users (email);
CREATE INDEX ix_users_role_status ON users (role, status);

CREATE TABLE sessions (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id      INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    token_hash   TEXT    NOT NULL,
    user_agent   TEXT    NOT NULL DEFAULT '',
    issued_at    INTEGER NOT NULL,
    expires_at   INTEGER NOT NULL,
    last_seen_at INTEGER NOT NULL,
    revoked_at   INTEGER NULL
);

CREATE UNIQUE INDEX ux_sessions_token_hash ON sessions (token_hash);
CREATE INDEX ix_sessions_user_active ON sessions (user_id, revoked_at, expires_at);

CREATE TABLE trails (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    code                TEXT    NOT NULL,
    name                TEXT    NOT NULL,
    region              TEXT    NOT NULL,
    difficulty          INTEGER NOT NULL CHECK (difficulty BETWEEN 1 AND 5),
    distance_km         REAL    NOT NULL CHECK (distance_km > 0),
    daily_quota         INTEGER NOT NULL CHECK (daily_quota > 0),
    min_party_size      INTEGER NOT NULL CHECK (min_party_size > 0),
    max_party_size      INTEGER NOT NULL CHECK (max_party_size >= min_party_size),
    permit_cutoff_hours INTEGER NOT NULL CHECK (permit_cutoff_hours >= 0),
    status              TEXT    NOT NULL CHECK (status IN ('open', 'season_closed', 'suspended')),
    version             INTEGER NOT NULL DEFAULT 1,
    created_at          INTEGER NOT NULL,
    updated_at          INTEGER NOT NULL
);

CREATE UNIQUE INDEX ux_trails_code ON trails (code);
CREATE INDEX ix_trails_region_status ON trails (region, status);

CREATE TABLE permit_windows (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    trail_id       INTEGER NOT NULL REFERENCES trails (id) ON DELETE CASCADE,
    hike_day       TEXT    NOT NULL,
    quota_total    INTEGER NOT NULL CHECK (quota_total >= 0),
    quota_reserved INTEGER NOT NULL DEFAULT 0 CHECK (quota_reserved >= 0 AND quota_reserved <= quota_total),
    version        INTEGER NOT NULL DEFAULT 1,
    closed_at      INTEGER NULL,
    created_at     INTEGER NOT NULL,
    updated_at     INTEGER NOT NULL
);

CREATE UNIQUE INDEX ux_permit_windows_trail_day ON permit_windows (trail_id, hike_day);
CREATE INDEX ix_permit_windows_day ON permit_windows (hike_day);

CREATE TABLE checkpoints (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    trail_id       INTEGER NOT NULL REFERENCES trails (id) ON DELETE CASCADE,
    seq            INTEGER NOT NULL CHECK (seq > 0),
    name           TEXT    NOT NULL,
    cutoff_minutes INTEGER NOT NULL CHECK (cutoff_minutes > 0),
    mandatory      INTEGER NOT NULL DEFAULT 1 CHECK (mandatory IN (0, 1)),
    created_at     INTEGER NOT NULL
);

CREATE UNIQUE INDEX ux_checkpoints_trail_seq ON checkpoints (trail_id, seq);
