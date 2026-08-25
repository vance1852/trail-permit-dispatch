// Package domain holds the entities, value objects and state machines of the
// trail permit dispatch platform. It depends on no transport or storage code.
package domain

import (
	"strings"
	"time"
	"unicode"

	"github.com/vance1852/trail-permit-dispatch/internal/apperr"
)

// Role is the business identity of an account. The platform intentionally keeps
// only two roles because permit governance and party leading are the two real
// responsibilities in the field.
type Role string

const (
	// RoleLeader is a hiking party leader. Leaders own their own parties only.
	RoleLeader Role = "leader"
	// RoleRanger is a trail authority operator governing permits and incidents.
	RoleRanger Role = "ranger"
)

// ParseRole validates an external role value.
func ParseRole(value string) (Role, error) {
	switch Role(strings.TrimSpace(strings.ToLower(value))) {
	case RoleLeader:
		return RoleLeader, nil
	case RoleRanger:
		return RoleRanger, nil
	default:
		return "", apperr.Newf(apperr.CodeInvalidArgument, "未知业务角色 %q", value).WithField("role")
	}
}

// Valid reports whether the role is one of the supported business roles.
func (r Role) Valid() bool {
	return r == RoleLeader || r == RoleRanger
}

// CanGovernPermits reports whether the role may open permit windows, release
// parties onto the trail, resolve incidents and settle permit fees.
func (r Role) CanGovernPermits() bool { return r == RoleRanger }

// CanLeadParties reports whether the role may create and run hiking parties.
func (r Role) CanLeadParties() bool { return r == RoleLeader }

// CanReadAudit reports whether the role may read the audit trail.
func (r Role) CanReadAudit() bool { return r == RoleRanger }

// UserStatus controls whether an account may authenticate at all.
type UserStatus string

const (
	// UserActive accounts may log in.
	UserActive UserStatus = "active"
	// UserSuspended accounts keep their history but cannot log in.
	UserSuspended UserStatus = "suspended"
)

// User is an account of the platform.
type User struct {
	ID           int64
	Email        string
	DisplayName  string
	Role         Role
	Status       UserStatus
	PasswordHash string
	PasswordSalt string
	KDFIter      int
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// Active reports whether the account may authenticate.
func (u *User) Active() bool { return u != nil && u.Status == UserActive }

// NormalizeEmail lower-cases and trims an email so uniqueness is stable.
func NormalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// ValidateEmail applies the minimal structural checks the platform needs.
func ValidateEmail(email string) error {
	normalized := NormalizeEmail(email)
	if normalized == "" {
		return apperr.New(apperr.CodeInvalidArgument, "邮箱不能为空").WithField("email")
	}
	if len(normalized) > 160 {
		return apperr.New(apperr.CodeInvalidArgument, "邮箱长度超过上限").WithField("email")
	}
	at := strings.IndexByte(normalized, '@')
	if at <= 0 || at == len(normalized)-1 {
		return apperr.New(apperr.CodeInvalidArgument, "邮箱格式不正确").WithField("email")
	}
	if strings.Contains(normalized[at+1:], "@") || !strings.Contains(normalized[at+1:], ".") {
		return apperr.New(apperr.CodeInvalidArgument, "邮箱格式不正确").WithField("email")
	}
	return nil
}

// ValidatePassword enforces the minimum credential strength of the platform.
func ValidatePassword(password string) error {
	if len(password) < 10 {
		return apperr.New(apperr.CodeInvalidArgument, "密码至少需要 10 个字符").WithField("password")
	}
	if len(password) > 200 {
		return apperr.New(apperr.CodeInvalidArgument, "密码长度超过上限").WithField("password")
	}
	var hasLetter, hasDigit bool
	for _, r := range password {
		switch {
		case unicode.IsLetter(r):
			hasLetter = true
		case unicode.IsDigit(r):
			hasDigit = true
		}
	}
	if !hasLetter || !hasDigit {
		return apperr.New(apperr.CodeInvalidArgument, "密码必须同时包含字母和数字").WithField("password")
	}
	return nil
}

// ValidateDisplayName checks the operator visible account name.
func ValidateDisplayName(name string) error {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return apperr.New(apperr.CodeInvalidArgument, "显示名称不能为空").WithField("display_name")
	}
	if len([]rune(trimmed)) > 40 {
		return apperr.New(apperr.CodeInvalidArgument, "显示名称超过 40 个字符").WithField("display_name")
	}
	return nil
}

// Session is a server side, revocable credential. Only the token hash is stored
// so a database dump never yields usable bearer tokens.
type Session struct {
	ID         int64
	UserID     int64
	TokenHash  string
	UserAgent  string
	IssuedAt   time.Time
	ExpiresAt  time.Time
	LastSeenAt time.Time
	RevokedAt  *time.Time
}

// Revoked reports whether the session was explicitly terminated.
func (s *Session) Revoked() bool { return s != nil && s.RevokedAt != nil }

// Expired reports whether the session outlived its time to live.
func (s *Session) Expired(now time.Time) bool {
	return s != nil && !now.Before(s.ExpiresAt)
}

// Active reports whether the session may still authenticate a request.
func (s *Session) Active(now time.Time) bool {
	return s != nil && !s.Revoked() && !s.Expired(now)
}

// Actor is the authenticated caller carried through the request context.
type Actor struct {
	UserID    int64
	Email     string
	Role      Role
	SessionID int64
}

// RequireRole verifies that the actor owns the expected business role.
func (a Actor) RequireRole(role Role) error {
	if a.UserID == 0 {
		return apperr.New(apperr.CodeUnauthenticated, "请先登录")
	}
	if a.Role != role {
		return apperr.Newf(apperr.CodePermissionDenied, "该操作仅限 %s 角色", role)
	}
	return nil
}
