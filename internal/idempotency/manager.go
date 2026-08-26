// Package idempotency stores the response of an accepted mutating request so a
// retried client request produces the same result instead of a second booking.
package idempotency

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	"github.com/vance1852/trail-permit-dispatch/internal/apperr"
	"github.com/vance1852/trail-permit-dispatch/internal/clock"
	"github.com/vance1852/trail-permit-dispatch/internal/repository"
)

// Manager coordinates idempotency keys for one scope namespace.
type Manager struct {
	repo  repository.IdempotencyRepository
	clock clock.Clock
	ttl   time.Duration
}

// New builds an idempotency manager.
func New(repo repository.IdempotencyRepository, clk clock.Clock, ttl time.Duration) *Manager {
	if clk == nil {
		clk = clock.System{}
	}
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	return &Manager{repo: repo, clock: clk, ttl: ttl}
}

// Hit is the outcome of an idempotency lookup.
type Hit struct {
	// Replay is true when a previous response must be returned unchanged.
	Replay bool
	// Body is the stored response payload of the earlier accepted request.
	Body string
}

// Lookup checks whether the key was already used. A reused key with a different
// request body is a client bug and is rejected as a conflict. An expired record
// is treated as unused so the retention window stays meaningful.
func (m *Manager) Lookup(ctx context.Context, scope, key string, actorID int64, requestHash string) (Hit, error) {
	if strings.TrimSpace(key) == "" {
		return Hit{}, nil
	}
	record, err := m.repo.Find(ctx, scope, key, actorID)
	if err != nil {
		if apperr.Is(err, apperr.CodeNotFound) {
			return Hit{}, nil
		}
		return Hit{}, err
	}
	now := m.clock.Now()
	if !record.ExpiresAt.IsZero() && !now.Before(record.ExpiresAt) {
		return Hit{}, nil
	}
	if record.RequestHash != requestHash {
		return Hit{}, apperr.Newf(apperr.CodeConflict,
			"幂等键 %s 已用于内容不同的请求，请更换幂等键", key).WithField("Idempotency-Key")
	}
	return Hit{Replay: true, Body: record.ResponseBody}, nil
}

// Remember stores the response of an accepted request. It must run inside the
// same transaction as the business write so the key and its effect commit
// together.
func (m *Manager) Remember(ctx context.Context, scope, key string, actorID int64, requestHash, responseBody string) error {
	if strings.TrimSpace(key) == "" {
		return nil
	}
	now := m.clock.Now()
	record := &repository.IdempotencyRecord{
		Scope:        scope,
		Key:          key,
		ActorID:      actorID,
		RequestHash:  requestHash,
		ResponseBody: responseBody,
		CreatedAt:    now,
		ExpiresAt:    now.Add(m.ttl),
	}
	if _, err := m.repo.Save(ctx, record); err != nil {
		if apperr.Is(err, apperr.CodeConflict) {
			return apperr.Newf(apperr.CodeConflict, "幂等键 %s 正在被并发使用，请稍后重试", key)
		}
		return err
	}
	return nil
}

// Purge removes expired records and reports how many were deleted.
func (m *Manager) Purge(ctx context.Context) (int, error) {
	return m.repo.DeleteExpired(ctx, m.clock.Now())
}

// Fingerprint renders a stable hash of a request payload.
func Fingerprint(payload any) (string, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInvalidArgument, "无法计算请求指纹", err)
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

// Encode renders a response payload for storage.
func Encode(payload any) (string, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "无法序列化幂等响应", err)
	}
	return string(encoded), nil
}

// Decode restores a stored response payload.
func Decode(body string, target any) error {
	if strings.TrimSpace(body) == "" {
		return apperr.New(apperr.CodeInternal, "幂等响应内容为空")
	}
	if err := json.Unmarshal([]byte(body), target); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "无法解析幂等响应", err)
	}
	return nil
}
