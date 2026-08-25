package sqliterepo

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/vance1852/trail-permit-dispatch/internal/apperr"
	"github.com/vance1852/trail-permit-dispatch/internal/clock"
	"github.com/vance1852/trail-permit-dispatch/internal/domain"
	"github.com/vance1852/trail-permit-dispatch/internal/repository"
	"github.com/vance1852/trail-permit-dispatch/internal/storage/sqlite"
)

// businessZone exposes the operating zone to the scan helpers.
func businessZone() *time.Location { return clock.Zone() }

// UserRepo is the SQLite backed account store.
type UserRepo struct {
	base
}

// NewUserRepo builds an account repository.
func NewUserRepo(tx *sqlite.TxManager) *UserRepo {
	return &UserRepo{base{tx: tx}}
}

const userColumns = `id, email, display_name, role, status, password_hash, password_salt, kdf_iterations, created_at, updated_at`

// Create inserts a new account.
func (r *UserRepo) Create(ctx context.Context, user *domain.User) (int64, error) {
	if user == nil {
		return 0, apperr.New(apperr.CodeInternal, "账号数据为空")
	}
	result, err := r.q(ctx).ExecContext(ctx,
		`INSERT INTO users (email, display_name, role, status, password_hash, password_salt, kdf_iterations, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		user.Email, user.DisplayName, string(user.Role), string(user.Status),
		user.PasswordHash, user.PasswordSalt, user.KDFIter,
		unixOrZero(user.CreatedAt), unixOrZero(user.UpdatedAt),
	)
	id, err := insertedID("创建账号", result, err)
	if err != nil {
		return 0, err
	}
	user.ID = id
	return id, nil
}

// ByID loads an account by identifier.
func (r *UserRepo) ByID(ctx context.Context, id int64) (*domain.User, error) {
	row := r.q(ctx).QueryRowContext(ctx, `SELECT `+userColumns+` FROM users WHERE id = ?`, id)
	return scanUser(row, "账号")
}

// ByEmail loads an account by its normalised email.
func (r *UserRepo) ByEmail(ctx context.Context, email string) (*domain.User, error) {
	row := r.q(ctx).QueryRowContext(ctx, `SELECT `+userColumns+` FROM users WHERE email = ?`, domain.NormalizeEmail(email))
	return scanUser(row, "账号")
}

// UpdateCredential rotates the stored password material.
func (r *UserRepo) UpdateCredential(ctx context.Context, userID int64, hash, salt string, iterations int, now time.Time) error {
	affected, err := affectedRows("更新账号密码", func() (sql.Result, error) {
		return r.q(ctx).ExecContext(ctx,
			`UPDATE users SET password_hash = ?, password_salt = ?, kdf_iterations = ?, updated_at = ? WHERE id = ?`,
			hash, salt, iterations, unixOrZero(now), userID)
	})
	if err != nil {
		return err
	}
	if affected == 0 {
		return notFound("账号")
	}
	return nil
}

// UpdateStatus suspends or reactivates an account.
func (r *UserRepo) UpdateStatus(ctx context.Context, userID int64, status domain.UserStatus, now time.Time) error {
	affected, err := affectedRows("更新账号状态", func() (sql.Result, error) {
		return r.q(ctx).ExecContext(ctx,
			`UPDATE users SET status = ?, updated_at = ? WHERE id = ?`,
			string(status), unixOrZero(now), userID)
	})
	if err != nil {
		return err
	}
	if affected == 0 {
		return notFound("账号")
	}
	return nil
}

// CountByRole counts accounts of one business role.
func (r *UserRepo) CountByRole(ctx context.Context, role domain.Role) (int, error) {
	return countQuery(ctx, r.q(ctx), "统计账号", `SELECT COUNT(1) FROM users WHERE role = ?`, string(role))
}

func scanUser(row *sql.Row, entity string) (*domain.User, error) {
	var user domain.User
	var role, status string
	var createdAt, updatedAt int64
	err := row.Scan(&user.ID, &user.Email, &user.DisplayName, &role, &status,
		&user.PasswordHash, &user.PasswordSalt, &user.KDFIter, &createdAt, &updatedAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, notFound(entity)
	case err != nil:
		return nil, translate("读取"+entity, err)
	}
	user.Role = domain.Role(role)
	user.Status = domain.UserStatus(status)
	user.CreatedAt = fromUnix(createdAt)
	user.UpdatedAt = fromUnix(updatedAt)
	return &user, nil
}

// SessionRepo is the SQLite backed session store.
type SessionRepo struct {
	base
}

// NewSessionRepo builds a session repository.
func NewSessionRepo(tx *sqlite.TxManager) *SessionRepo {
	return &SessionRepo{base{tx: tx}}
}

const sessionColumns = `id, user_id, token_hash, user_agent, issued_at, expires_at, last_seen_at, revoked_at`

// Create stores a new session.
func (r *SessionRepo) Create(ctx context.Context, session *domain.Session) (int64, error) {
	if session == nil {
		return 0, apperr.New(apperr.CodeInternal, "会话数据为空")
	}
	result, err := r.q(ctx).ExecContext(ctx,
		`INSERT INTO sessions (user_id, token_hash, user_agent, issued_at, expires_at, last_seen_at, revoked_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		session.UserID, session.TokenHash, session.UserAgent,
		unixOrZero(session.IssuedAt), unixOrZero(session.ExpiresAt),
		unixOrZero(session.LastSeenAt), unixPtr(session.RevokedAt),
	)
	id, err := insertedID("创建会话", result, err)
	if err != nil {
		return 0, err
	}
	session.ID = id
	return id, nil
}

// ByTokenHash loads a session by the hash of its bearer token.
func (r *SessionRepo) ByTokenHash(ctx context.Context, tokenHash string) (*domain.Session, error) {
	row := r.q(ctx).QueryRowContext(ctx, `SELECT `+sessionColumns+` FROM sessions WHERE token_hash = ?`, tokenHash)
	return scanSession(row)
}

// ByID loads a session by identifier.
func (r *SessionRepo) ByID(ctx context.Context, id int64) (*domain.Session, error) {
	row := r.q(ctx).QueryRowContext(ctx, `SELECT `+sessionColumns+` FROM sessions WHERE id = ?`, id)
	return scanSession(row)
}

// TouchLastSeen records activity on an active session.
func (r *SessionRepo) TouchLastSeen(ctx context.Context, id int64, at time.Time) error {
	_, err := affectedRows("更新会话活跃时间", func() (sql.Result, error) {
		return r.q(ctx).ExecContext(ctx,
			`UPDATE sessions SET last_seen_at = ? WHERE id = ? AND revoked_at IS NULL`,
			unixOrZero(at), id)
	})
	return err
}

// Revoke terminates one session and reports whether it was still active.
func (r *SessionRepo) Revoke(ctx context.Context, id int64, at time.Time) (bool, error) {
	affected, err := affectedRows("撤销会话", func() (sql.Result, error) {
		return r.q(ctx).ExecContext(ctx,
			`UPDATE sessions SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`,
			unixOrZero(at), id)
	})
	if err != nil {
		return false, err
	}
	return affected > 0, nil
}

// RevokeAllForUser terminates every active session of one account.
func (r *SessionRepo) RevokeAllForUser(ctx context.Context, userID int64, at time.Time) (int, error) {
	affected, err := affectedRows("撤销账号全部会话", func() (sql.Result, error) {
		return r.q(ctx).ExecContext(ctx,
			`UPDATE sessions SET revoked_at = ? WHERE user_id = ? AND revoked_at IS NULL`,
			unixOrZero(at), userID)
	})
	if err != nil {
		return 0, err
	}
	return int(affected), nil
}

// RevokeExpired revokes sessions that outlived their time to live.
func (r *SessionRepo) RevokeExpired(ctx context.Context, now time.Time) (int, error) {
	affected, err := affectedRows("清理过期会话", func() (sql.Result, error) {
		return r.q(ctx).ExecContext(ctx,
			`UPDATE sessions SET revoked_at = ? WHERE revoked_at IS NULL AND expires_at <= ?`,
			unixOrZero(now), unixOrZero(now))
	})
	if err != nil {
		return 0, err
	}
	return int(affected), nil
}

// CountActive counts sessions that can still authenticate a request.
func (r *SessionRepo) CountActive(ctx context.Context, userID int64, now time.Time) (int, error) {
	return countQuery(ctx, r.q(ctx), "统计活跃会话",
		`SELECT COUNT(1) FROM sessions WHERE user_id = ? AND revoked_at IS NULL AND expires_at > ?`,
		userID, unixOrZero(now))
}

func scanSession(row *sql.Row) (*domain.Session, error) {
	var session domain.Session
	var issuedAt, expiresAt, lastSeenAt int64
	var revokedAt sql.NullInt64
	err := row.Scan(&session.ID, &session.UserID, &session.TokenHash, &session.UserAgent,
		&issuedAt, &expiresAt, &lastSeenAt, &revokedAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, notFound("会话")
	case err != nil:
		return nil, translate("读取会话", err)
	}
	session.IssuedAt = fromUnix(issuedAt)
	session.ExpiresAt = fromUnix(expiresAt)
	session.LastSeenAt = fromUnix(lastSeenAt)
	session.RevokedAt = fromNullUnix(revokedAt)
	return &session, nil
}

// compile time assertions that the SQLite implementations satisfy the contracts.
var (
	_ repository.UserRepository    = (*UserRepo)(nil)
	_ repository.SessionRepository = (*SessionRepo)(nil)
)
