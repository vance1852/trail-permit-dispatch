package idempotency

import (
	"context"
	"testing"
	"time"

	"github.com/vance1852/trail-permit-dispatch/internal/apperr"
	"github.com/vance1852/trail-permit-dispatch/internal/clock"
	"github.com/vance1852/trail-permit-dispatch/internal/repository"
)

// fakeStore is an in-memory idempotency store.
type fakeStore struct {
	records map[string]repository.IdempotencyRecord
	nextID  int64
}

func newFakeStore() *fakeStore {
	return &fakeStore{records: make(map[string]repository.IdempotencyRecord, 4)}
}

func key(scope, idemKey string, actorID int64) string {
	return scope + "|" + idemKey + "|" + string(rune('0'+actorID))
}

func (f *fakeStore) Find(_ context.Context, scope, idemKey string, actorID int64) (*repository.IdempotencyRecord, error) {
	record, ok := f.records[key(scope, idemKey, actorID)]
	if !ok {
		return nil, apperr.New(apperr.CodeNotFound, "幂等记录不存在")
	}
	copied := record
	return &copied, nil
}

func (f *fakeStore) Save(_ context.Context, record *repository.IdempotencyRecord) (int64, error) {
	storeKey := key(record.Scope, record.Key, record.ActorID)
	if _, exists := f.records[storeKey]; exists {
		return 0, apperr.New(apperr.CodeConflict, "幂等键重复")
	}
	f.nextID++
	record.ID = f.nextID
	f.records[storeKey] = *record
	return record.ID, nil
}

func (f *fakeStore) DeleteExpired(_ context.Context, now time.Time) (int, error) {
	deleted := 0
	for storeKey, record := range f.records {
		if !record.ExpiresAt.After(now) {
			delete(f.records, storeKey)
			deleted++
		}
	}
	return deleted, nil
}

func TestLookupReturnsMissWithoutKey(t *testing.T) {
	store := newFakeStore()
	manager := New(store, clock.NewFixed(time.Now()), time.Hour)
	hit, err := manager.Lookup(context.Background(), "party.permit_request", "  ", 1, "hash")
	if err != nil {
		t.Fatalf("空幂等键不应报错: %v", err)
	}
	if hit.Replay {
		t.Fatal("空幂等键不应触发重放")
	}
	if err := manager.Remember(context.Background(), "party.permit_request", "", 1, "hash", "{}"); err != nil {
		t.Fatalf("空幂等键的记忆操作应被忽略: %v", err)
	}
	if len(store.records) != 0 {
		t.Fatal("空幂等键不应写入记录")
	}
}

func TestRememberAndReplay(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	fixed := clock.NewFixed(time.Date(2026, 9, 11, 8, 0, 0, 0, clock.Zone()))
	manager := New(store, fixed, time.Hour)

	if err := manager.Remember(ctx, "party.permit_request", "k1", 1, "hash-1", `{"code":"TP-1"}`); err != nil {
		t.Fatalf("保存幂等响应失败: %v", err)
	}
	hit, err := manager.Lookup(ctx, "party.permit_request", "k1", 1, "hash-1")
	if err != nil {
		t.Fatalf("读取幂等响应失败: %v", err)
	}
	if !hit.Replay || hit.Body != `{"code":"TP-1"}` {
		t.Fatalf("重放内容错误: %+v", hit)
	}

	if _, err := manager.Lookup(ctx, "party.permit_request", "k1", 1, "hash-2"); !apperr.Is(err, apperr.CodeConflict) {
		t.Fatalf("请求内容不一致应返回 conflict，实际 %v", err)
	}
	other, err := manager.Lookup(ctx, "party.permit_request", "k1", 2, "hash-1")
	if err != nil {
		t.Fatalf("不同操作者的查询失败: %v", err)
	}
	if other.Replay {
		t.Fatal("幂等键应按操作者隔离")
	}
	scoped, err := manager.Lookup(ctx, "party.cancel", "k1", 1, "hash-1")
	if err != nil {
		t.Fatalf("不同作用域的查询失败: %v", err)
	}
	if scoped.Replay {
		t.Fatal("幂等键应按作用域隔离")
	}
	if err := manager.Remember(ctx, "party.permit_request", "k1", 1, "hash-1", "{}"); !apperr.Is(err, apperr.CodeConflict) {
		t.Fatalf("重复写入同一幂等键应返回 conflict，实际 %v", err)
	}
}

func TestExpiredRecordIsTreatedAsUnused(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	fixed := clock.NewFixed(time.Date(2026, 9, 11, 8, 0, 0, 0, clock.Zone()))
	manager := New(store, fixed, time.Minute)

	if err := manager.Remember(ctx, "party.permit_request", "k1", 1, "hash-1", "{}"); err != nil {
		t.Fatalf("保存幂等响应失败: %v", err)
	}
	fixed.Advance(2 * time.Minute)
	hit, err := manager.Lookup(ctx, "party.permit_request", "k1", 1, "hash-1")
	if err != nil {
		t.Fatalf("过期记录查询失败: %v", err)
	}
	if hit.Replay {
		t.Fatal("过期记录不应继续重放")
	}
	purged, err := manager.Purge(ctx)
	if err != nil {
		t.Fatalf("清理幂等记录失败: %v", err)
	}
	if purged != 1 {
		t.Fatalf("应清理 1 条记录，实际 %d", purged)
	}
}

func TestFingerprintAndCodec(t *testing.T) {
	first, err := Fingerprint(map[string]any{"party": "TP-1", "size": 3})
	if err != nil {
		t.Fatalf("计算指纹失败: %v", err)
	}
	same, err := Fingerprint(map[string]any{"size": 3, "party": "TP-1"})
	if err != nil {
		t.Fatalf("计算指纹失败: %v", err)
	}
	if first != same {
		t.Fatal("字段顺序不应影响指纹")
	}
	different, err := Fingerprint(map[string]any{"party": "TP-1", "size": 4})
	if err != nil {
		t.Fatalf("计算指纹失败: %v", err)
	}
	if first == different {
		t.Fatal("不同请求内容应产生不同指纹")
	}
	if len(first) != 64 {
		t.Fatalf("指纹应为 64 位十六进制，实际 %d", len(first))
	}
	if _, err := Fingerprint(func() {}); !apperr.Is(err, apperr.CodeInvalidArgument) {
		t.Fatalf("不可序列化的载荷应被拒绝，实际 %v", err)
	}

	encoded, err := Encode(map[string]string{"code": "TP-1"})
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	var decoded map[string]string
	if err := Decode(encoded, &decoded); err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}
	if decoded["code"] != "TP-1" {
		t.Fatalf("反序列化结果错误: %+v", decoded)
	}
	if err := Decode("  ", &decoded); !apperr.Is(err, apperr.CodeInternal) {
		t.Fatalf("空响应体应返回 internal，实际 %v", err)
	}
	if err := Decode("not json", &decoded); !apperr.Is(err, apperr.CodeInternal) {
		t.Fatalf("非法响应体应返回 internal，实际 %v", err)
	}
}

func TestNewAppliesDefaults(t *testing.T) {
	manager := New(newFakeStore(), nil, 0)
	if manager.ttl != 24*time.Hour {
		t.Fatalf("默认保留期错误: %s", manager.ttl)
	}
	if manager.clock == nil {
		t.Fatal("默认时钟不应为空")
	}
}

var _ repository.IdempotencyRepository = (*fakeStore)(nil)
