package sqliterepo

import (
	"context"
	"testing"
	"time"

	"github.com/vance1852/trail-permit-dispatch/internal/clock"
	"github.com/vance1852/trail-permit-dispatch/internal/domain"
	"github.com/vance1852/trail-permit-dispatch/internal/idempotency"
)

// TestManagerIdempotencyKeysAreScopedPerLeader exercises the production path:
// the real IdempotencyRepo behind the real idempotency.Manager. Two leaders
// submitting the same key for different parties must each succeed.
func TestManagerIdempotencyKeysAreScopedPerLeader(t *testing.T) {
	ctx := context.Background()
	fix := newFixture(t)
	leaderA := fix.newUser("leaderA@trail.local", domain.RoleLeader)
	leaderB := fix.newUser("leaderB@trail.local", domain.RoleLeader)

	fixed := clock.NewFixed(testNow())
	manager := idempotency.New(fix.idem, fixed, time.Hour)

	if err := manager.Remember(ctx, "party.permit_request", "shared-key", leaderA.ID, "hash-A", `{"code":"TP-A"}`); err != nil {
		t.Fatalf("领队 A 记录幂等响应失败: %v", err)
	}
	// Leader B submits the SAME key but a DIFFERENT request body. In production
	// this was rejected with a conflict because A's record leaked to B.
	if err := manager.Remember(ctx, "party.permit_request", "shared-key", leaderB.ID, "hash-B", `{"code":"TP-B"}`); err != nil {
		t.Fatalf("领队 B 用同一编号提交不同请求应成功，实际 %v", err)
	}

	// Leader A replays: same key, same body, must dedupe.
	if _, err := manager.Lookup(ctx, "party.permit_request", "shared-key", leaderA.ID, "hash-A"); err != nil {
		t.Fatalf("领队 A 重放失败: %v", err)
	}
	// Leader B looks up its own record.
	bHit, err := manager.Lookup(ctx, "party.permit_request", "shared-key", leaderB.ID, "hash-B")
	if err != nil {
		t.Fatalf("领队 B 查询失败: %v", err)
	}
	if bHit.Body != `{"code":"TP-B"}` {
		t.Fatalf("领队 B 命中的应是自己的响应，实际 %s", bHit.Body)
	}

	// Same leader resubmits the identical key+body: conflict (already used once).
	if err := manager.Remember(ctx, "party.permit_request", "shared-key", leaderA.ID, "hash-A", `{"code":"TP-A-replay"}`); err == nil {
		t.Fatal("同一领队重复写入同一幂等键应被拒绝")
	}
}
