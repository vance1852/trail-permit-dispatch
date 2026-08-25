// Package auth implements login, revocable sessions, logout and the account
// lifecycle rules of the platform.
package auth

import (
	"context"
	"time"

	"github.com/vance1852/trail-permit-dispatch/internal/apperr"
	"github.com/vance1852/trail-permit-dispatch/internal/audit"
	"github.com/vance1852/trail-permit-dispatch/internal/clock"
	"github.com/vance1852/trail-permit-dispatch/internal/domain"
	"github.com/vance1852/trail-permit-dispatch/internal/repository"
	"github.com/vance1852/trail-permit-dispatch/internal/security"
)

// Audit actions produced by this service.
const (
	ActionLogin       = "auth.login"
	ActionLogout      = "auth.logout"
	ActionRegister    = "auth.register"
	ActionRevokeAll   = "auth.revoke_all"
	ActionExpireSweep = "auth.expire_sessions"
)

// Deps carries the collaborators of the authentication service.
type Deps struct {
	Tx         repository.TxRunner
	Users      repository.UserRepository
	Sessions   repository.SessionRepository
	Audit      *audit.Recorder
	Hasher     *security.Hasher
	Clock      clock.Clock
	SessionTTL time.Duration
}

// Service owns the identity lifecycle.
type Service struct {
	tx         repository.TxRunner
	users      repository.UserRepository
	sessions   repository.SessionRepository
	audit      *audit.Recorder
	hasher     *security.Hasher
	clock      clock.Clock
	sessionTTL time.Duration
}

// New builds an authentication service.
func New(deps Deps) *Service {
	ttl := deps.SessionTTL
	if ttl <= 0 {
		ttl = 12 * time.Hour
	}
	clk := deps.Clock
	if clk == nil {
		clk = clock.System{}
	}
	hasher := deps.Hasher
	if hasher == nil {
		hasher = security.NewHasher(security.DefaultIterations)
	}
	return &Service{
		tx:         deps.Tx,
		users:      deps.Users,
		sessions:   deps.Sessions,
		audit:      deps.Audit,
		hasher:     hasher,
		clock:      clk,
		sessionTTL: ttl,
	}
}

// AccountView is the public projection of an account.
type AccountView struct {
	ID          int64  `json:"id"`
	Email       string `json:"email"`
	DisplayName string `json:"display_name"`
	Role        string `json:"role"`
	Status      string `json:"status"`
}

// SessionView is the public projection of a session.
type SessionView struct {
	SessionID int64     `json:"session_id"`
	IssuedAt  time.Time `json:"issued_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// LoginResult is returned by a successful login.
type LoginResult struct {
	Token   string      `json:"token"`
	Session SessionView `json:"session"`
	Account AccountView `json:"account"`
}

// RegisterInput describes a new account.
type RegisterInput struct {
	Email       string
	DisplayName string
	Password    string
	Role        string
}

// Register creates an account. Only a ranger may create accounts; the very
// first ranger is created by the bootstrap seed instead.
func (s *Service) Register(ctx context.Context, actor domain.Actor, input RegisterInput) (AccountView, error) {
	if err := actor.RequireRole(domain.RoleRanger); err != nil {
		return AccountView{}, err
	}
	role, err := domain.ParseRole(input.Role)
	if err != nil {
		return AccountView{}, err
	}
	view, err := s.createAccount(ctx, input.Email, input.DisplayName, input.Password, role)
	if err != nil {
		return AccountView{}, err
	}
	if err := s.audit.Success(ctx, actor, ActionRegister, "account", audit.ObjectID(view.ID),
		"创建账号 "+view.Email+" 角色 "+view.Role); err != nil {
		return AccountView{}, err
	}
	return view, nil
}

// Provision creates an account without an authenticated actor. It is used by the
// bootstrap seed of an empty database only.
func (s *Service) Provision(ctx context.Context, email, displayName, password string, role domain.Role) (AccountView, error) {
	return s.createAccount(ctx, email, displayName, password, role)
}

func (s *Service) createAccount(ctx context.Context, email, displayName, password string, role domain.Role) (AccountView, error) {
	if err := domain.ValidateEmail(email); err != nil {
		return AccountView{}, err
	}
	if err := domain.ValidateDisplayName(displayName); err != nil {
		return AccountView{}, err
	}
	if err := domain.ValidatePassword(password); err != nil {
		return AccountView{}, err
	}
	if !role.Valid() {
		return AccountView{}, apperr.Newf(apperr.CodeInvalidArgument, "未知业务角色 %q", role).WithField("role")
	}
	credential, err := s.hasher.Hash(password)
	if err != nil {
		return AccountView{}, err
	}
	now := clock.Truncate(s.clock.Now())
	user := &domain.User{
		Email:        domain.NormalizeEmail(email),
		DisplayName:  displayName,
		Role:         role,
		Status:       domain.UserActive,
		PasswordHash: credential.Hash,
		PasswordSalt: credential.Salt,
		KDFIter:      credential.Iterations,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if _, err := s.users.Create(ctx, user); err != nil {
		if apperr.Is(err, apperr.CodeConflict) {
			return AccountView{}, apperr.Newf(apperr.CodeConflict, "邮箱 %s 已被注册", user.Email).WithField("email")
		}
		return AccountView{}, err
	}
	return toAccountView(user), nil
}

// Login verifies the credential and mints a revocable session token.
func (s *Service) Login(ctx context.Context, email, password, userAgent string) (LoginResult, error) {
	if err := domain.ValidateEmail(email); err != nil {
		return LoginResult{}, err
	}
	if password == "" {
		return LoginResult{}, apperr.New(apperr.CodeInvalidArgument, "密码不能为空").WithField("password")
	}
	user, err := s.users.ByEmail(ctx, email)
	if err != nil {
		if apperr.Is(err, apperr.CodeNotFound) {
			return LoginResult{}, apperr.New(apperr.CodeUnauthenticated, "邮箱或密码不正确")
		}
		return LoginResult{}, err
	}
	if err := s.hasher.Verify(security.Credential{
		Hash:       user.PasswordHash,
		Salt:       user.PasswordSalt,
		Iterations: user.KDFIter,
	}, password); err != nil {
		return LoginResult{}, err
	}
	if !user.Active() {
		return LoginResult{}, apperr.New(apperr.CodePermissionDenied, "账号已被停用，请联系线路管理员")
	}

	token, err := security.NewToken()
	if err != nil {
		return LoginResult{}, err
	}
	now := clock.Truncate(s.clock.Now())
	session := &domain.Session{
		UserID:     user.ID,
		TokenHash:  token.Hash,
		UserAgent:  trimUserAgent(userAgent),
		IssuedAt:   now,
		ExpiresAt:  now.Add(s.sessionTTL),
		LastSeenAt: now,
	}
	actor := domain.Actor{UserID: user.ID, Email: user.Email, Role: user.Role}
	err = s.tx.WithTx(ctx, func(txCtx context.Context) error {
		if _, err := s.sessions.Create(txCtx, session); err != nil {
			return err
		}
		actor.SessionID = session.ID
		return s.audit.Success(txCtx, actor, ActionLogin, audit.ObjectSession,
			audit.ObjectID(session.ID), "签发会话，有效期至 "+session.ExpiresAt.Format(time.RFC3339))
	})
	if err != nil {
		return LoginResult{}, err
	}
	return LoginResult{
		Token: token.Plaintext,
		Session: SessionView{
			SessionID: session.ID,
			IssuedAt:  session.IssuedAt,
			ExpiresAt: session.ExpiresAt,
		},
		Account: toAccountView(user),
	}, nil
}

// Authenticate resolves a bearer token into an actor. A revoked or expired
// session is rejected with the same error as an unknown token so an attacker
// learns nothing from the difference.
func (s *Service) Authenticate(ctx context.Context, token string) (domain.Actor, error) {
	if token == "" {
		return domain.Actor{}, apperr.New(apperr.CodeUnauthenticated, "缺少访问令牌")
	}
	session, err := s.sessions.ByTokenHash(ctx, security.HashToken(token))
	if err != nil {
		if apperr.Is(err, apperr.CodeNotFound) {
			return domain.Actor{}, apperr.New(apperr.CodeUnauthenticated, "访问令牌无效或已失效")
		}
		return domain.Actor{}, err
	}
	now := s.clock.Now()
	// 会话可用性已经由仓储层过滤，这里只需要继续校验账号状态。
	user, err := s.users.ByID(ctx, session.UserID)
	if err != nil {
		return domain.Actor{}, err
	}
	if !user.Active() {
		return domain.Actor{}, apperr.New(apperr.CodePermissionDenied, "账号已被停用，请联系线路管理员")
	}
	if err := s.sessions.TouchLastSeen(ctx, session.ID, clock.Truncate(now)); err != nil {
		return domain.Actor{}, err
	}
	return domain.Actor{UserID: user.ID, Email: user.Email, Role: user.Role, SessionID: session.ID}, nil
}

// Logout revokes the session of the current actor. Revoking an already revoked
// session is reported so a client can distinguish a double logout.
func (s *Service) Logout(ctx context.Context, actor domain.Actor) error {
	if actor.SessionID == 0 {
		return apperr.New(apperr.CodeUnauthenticated, "当前请求没有可撤销的会话")
	}
	return s.tx.WithTx(ctx, func(txCtx context.Context) error {
		revoked, err := s.sessions.Revoke(txCtx, actor.SessionID, clock.Truncate(s.clock.Now()))
		if err != nil {
			return err
		}
		if !revoked {
			return apperr.New(apperr.CodeStateInvalid, "会话已经失效")
		}
		return s.audit.Success(txCtx, actor, ActionLogout, audit.ObjectSession,
			audit.ObjectID(actor.SessionID), "撤销当前会话")
	})
}

// RevokeAll revokes every session of the actor, for example after a lost phone.
func (s *Service) RevokeAll(ctx context.Context, actor domain.Actor) (int, error) {
	if actor.UserID == 0 {
		return 0, apperr.New(apperr.CodeUnauthenticated, "请先登录")
	}
	var revoked int
	err := s.tx.WithTx(ctx, func(txCtx context.Context) error {
		count, err := s.sessions.RevokeAllForUser(txCtx, actor.UserID, clock.Truncate(s.clock.Now()))
		if err != nil {
			return err
		}
		revoked = count
		return s.audit.Success(txCtx, actor, ActionRevokeAll, audit.ObjectSession,
			audit.ObjectID(actor.UserID), "撤销该账号全部会话")
	})
	if err != nil {
		return 0, err
	}
	return revoked, nil
}

// ExpireSessions revokes every session past its time to live. The worker calls
// it periodically so the sessions table cannot grow into a list of live tokens.
func (s *Service) ExpireSessions(ctx context.Context) (int, error) {
	var revoked int
	err := s.tx.WithTx(ctx, func(txCtx context.Context) error {
		count, err := s.sessions.RevokeExpired(txCtx, clock.Truncate(s.clock.Now()))
		if err != nil {
			return err
		}
		revoked = count
		if count == 0 {
			return nil
		}
		return s.audit.Success(txCtx, audit.SystemActor(), ActionExpireSweep, audit.ObjectSession,
			"batch", "清理过期会话")
	})
	if err != nil {
		return 0, err
	}
	return revoked, nil
}

// Account returns the account of the current actor.
func (s *Service) Account(ctx context.Context, actor domain.Actor) (AccountView, error) {
	if actor.UserID == 0 {
		return AccountView{}, apperr.New(apperr.CodeUnauthenticated, "请先登录")
	}
	user, err := s.users.ByID(ctx, actor.UserID)
	if err != nil {
		return AccountView{}, err
	}
	return toAccountView(user), nil
}

// ActiveSessions counts the still usable sessions of the current actor.
func (s *Service) ActiveSessions(ctx context.Context, actor domain.Actor) (int, error) {
	if actor.UserID == 0 {
		return 0, apperr.New(apperr.CodeUnauthenticated, "请先登录")
	}
	return s.sessions.CountActive(ctx, actor.UserID, s.clock.Now())
}

// CountRole reports how many accounts hold a business role.
func (s *Service) CountRole(ctx context.Context, role domain.Role) (int, error) {
	return s.users.CountByRole(ctx, role)
}

func toAccountView(user *domain.User) AccountView {
	return AccountView{
		ID:          user.ID,
		Email:       user.Email,
		DisplayName: user.DisplayName,
		Role:        string(user.Role),
		Status:      string(user.Status),
	}
}

func trimUserAgent(value string) string {
	const limit = 200
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}
