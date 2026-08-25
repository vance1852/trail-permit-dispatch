package apptest

import (
	"testing"
	"time"

	"github.com/vance1852/trail-permit-dispatch/internal/apperr"
)

// TestExpiredSessionTokenIsRejected 覆盖会话有效期到点后的鉴权行为：
// 过期令牌必须被拒绝、能被清理，同时重新登录与主动退出的行为保持正常。
func TestExpiredSessionTokenIsRejected(t *testing.T) {
	h := newHarness(t)

	token := h.token(leaderEmail, leaderPassword)
	actor, err := h.app.Auth.Authenticate(h.ctx(), token)
	if err != nil {
		t.Fatalf("新签发的令牌应可用: %v", err)
	}
	if actor.UserID == 0 || actor.SessionID == 0 {
		t.Fatalf("解析出的操作者信息不完整: %+v", actor)
	}

	// 会话有效期为 72 小时，推进 73 小时后必须失效。
	h.clock.Advance(73 * time.Hour)
	expired, err := h.app.Auth.Authenticate(h.ctx(), token)
	if err == nil {
		t.Fatalf("超过有效期的令牌必须被拒绝，实际却解析出操作者 %+v", expired)
	}
	if !apperr.Is(err, apperr.CodeUnauthenticated) {
		t.Fatalf("过期令牌应返回 unauthenticated，实际错误码 %s (%v)", apperr.CodeOf(err), err)
	}

	revoked, err := h.app.Auth.ExpireSessions(h.ctx())
	if err != nil {
		t.Fatalf("清理过期会话失败: %v", err)
	}
	if revoked != 1 {
		t.Fatalf("应清理 1 个过期会话，实际 %d", revoked)
	}
	if _, err := h.app.Auth.Authenticate(h.ctx(), token); !apperr.Is(err, apperr.CodeUnauthenticated) {
		t.Fatalf("清理后过期令牌仍必须被拒绝，实际 %v", err)
	}

	freshToken := h.token(leaderEmail, leaderPassword)
	freshActor, err := h.app.Auth.Authenticate(h.ctx(), freshToken)
	if err != nil {
		t.Fatalf("重新登录后的令牌应可用: %v", err)
	}
	active, err := h.app.Auth.ActiveSessions(h.ctx(), freshActor)
	if err != nil {
		t.Fatalf("统计活跃会话失败: %v", err)
	}
	if active != 1 {
		t.Fatalf("过期会话被清理后活跃会话应为 1，实际 %d", active)
	}

	if err := h.app.Auth.Logout(h.ctx(), freshActor); err != nil {
		t.Fatalf("退出失败: %v", err)
	}
	if _, err := h.app.Auth.Authenticate(h.ctx(), freshToken); !apperr.Is(err, apperr.CodeUnauthenticated) {
		t.Fatalf("退出后令牌必须立即失效，实际 %v", err)
	}
}
