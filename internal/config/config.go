// Package config loads the runtime configuration from the environment. No
// secret has a usable production default and every value is validated once at
// startup so the rest of the code can trust it.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/vance1852/trail-permit-dispatch/internal/apperr"
)

// Config is the validated runtime configuration of the server.
type Config struct {
	HTTPAddr              string
	DatabaseDSN           string
	LogLevel              string
	SessionTTL            time.Duration
	ShutdownTimeout       time.Duration
	ReadHeaderTimeout     time.Duration
	WorkerCount           int
	WorkerPollInterval    time.Duration
	WorkerJobTimeout      time.Duration
	JobLeaseDuration      time.Duration
	JobBackoffBase        time.Duration
	JobBackoffMax         time.Duration
	JobMaxAttempts        int
	CheckpointGraceMin    int
	PermitUnitFeeCents    int64
	IdempotencyTTL        time.Duration
	SeedEnabled           bool
	SeedRangerEmail       string
	SeedRangerPassword    string
	SeedLeaderEmail       string
	SeedLeaderPassword    string
	PasswordKDFIterations int
}

// Load reads and validates the configuration from the process environment.
func Load() (Config, error) {
	cfg := Config{
		HTTPAddr:              envString("HTTP_ADDR", ":8080"),
		DatabaseDSN:           envString("DATABASE_DSN", "file:data/trail-permit.sqlite"),
		LogLevel:              strings.ToLower(envString("LOG_LEVEL", "info")),
		SeedRangerEmail:       envString("SEED_RANGER_EMAIL", "ranger@trailpermit.local"),
		SeedLeaderEmail:       envString("SEED_LEADER_EMAIL", "leader@trailpermit.local"),
		PasswordKDFIterations: 120000,
		// Seed passwords deliberately have no default. When the operator does
		// not supply one, the bootstrap seed mints a one-time credential and
		// logs it once, so no usable password is ever committed.
		SeedRangerPassword: envString("SEED_RANGER_PASSWORD", ""),
		SeedLeaderPassword: envString("SEED_LEADER_PASSWORD", ""),
	}

	var err error
	if cfg.SessionTTL, err = envDuration("SESSION_TTL", 12*time.Hour); err != nil {
		return Config{}, err
	}
	if cfg.ShutdownTimeout, err = envDuration("SHUTDOWN_TIMEOUT", 15*time.Second); err != nil {
		return Config{}, err
	}
	if cfg.ReadHeaderTimeout, err = envDuration("READ_HEADER_TIMEOUT", 10*time.Second); err != nil {
		return Config{}, err
	}
	if cfg.WorkerPollInterval, err = envDuration("WORKER_POLL_INTERVAL", 500*time.Millisecond); err != nil {
		return Config{}, err
	}
	if cfg.WorkerJobTimeout, err = envDuration("WORKER_JOB_TIMEOUT", 20*time.Second); err != nil {
		return Config{}, err
	}
	if cfg.JobLeaseDuration, err = envDuration("JOB_LEASE_DURATION", 30*time.Second); err != nil {
		return Config{}, err
	}
	if cfg.JobBackoffBase, err = envDuration("JOB_BACKOFF_BASE", 2*time.Second); err != nil {
		return Config{}, err
	}
	if cfg.JobBackoffMax, err = envDuration("JOB_BACKOFF_MAX", 5*time.Minute); err != nil {
		return Config{}, err
	}
	if cfg.IdempotencyTTL, err = envDuration("IDEMPOTENCY_TTL", 24*time.Hour); err != nil {
		return Config{}, err
	}
	if cfg.WorkerCount, err = envInt("WORKER_COUNT", 2); err != nil {
		return Config{}, err
	}
	if cfg.JobMaxAttempts, err = envInt("JOB_MAX_ATTEMPTS", 5); err != nil {
		return Config{}, err
	}
	if cfg.CheckpointGraceMin, err = envInt("CHECKPOINT_GRACE_MINUTES", 30); err != nil {
		return Config{}, err
	}
	unitFee, err := envInt("PERMIT_UNIT_FEE_CENTS", 4500)
	if err != nil {
		return Config{}, err
	}
	cfg.PermitUnitFeeCents = int64(unitFee)
	if cfg.SeedEnabled, err = envBool("SEED_ENABLED", true); err != nil {
		return Config{}, err
	}

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate rejects configurations the server cannot run with.
func (c Config) Validate() error {
	if strings.TrimSpace(c.HTTPAddr) == "" {
		return apperr.New(apperr.CodeInvalidArgument, "HTTP_ADDR 不能为空")
	}
	if strings.TrimSpace(c.DatabaseDSN) == "" {
		return apperr.New(apperr.CodeInvalidArgument, "DATABASE_DSN 不能为空")
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return apperr.Newf(apperr.CodeInvalidArgument, "LOG_LEVEL %q 不受支持", c.LogLevel)
	}
	if c.SessionTTL < time.Minute {
		return apperr.New(apperr.CodeInvalidArgument, "SESSION_TTL 不能小于 1 分钟")
	}
	if c.WorkerCount < 1 || c.WorkerCount > 32 {
		return apperr.New(apperr.CodeInvalidArgument, "WORKER_COUNT 必须在 1 到 32 之间")
	}
	if c.WorkerPollInterval < 10*time.Millisecond {
		return apperr.New(apperr.CodeInvalidArgument, "WORKER_POLL_INTERVAL 不能小于 10ms")
	}
	if c.JobLeaseDuration <= c.WorkerPollInterval {
		return apperr.New(apperr.CodeInvalidArgument, "JOB_LEASE_DURATION 必须大于轮询间隔")
	}
	if c.JobMaxAttempts < 1 || c.JobMaxAttempts > 20 {
		return apperr.New(apperr.CodeInvalidArgument, "JOB_MAX_ATTEMPTS 必须在 1 到 20 之间")
	}
	if c.JobBackoffBase <= 0 || c.JobBackoffMax < c.JobBackoffBase {
		return apperr.New(apperr.CodeInvalidArgument, "作业退避区间配置无效")
	}
	if c.CheckpointGraceMin < 0 || c.CheckpointGraceMin > 240 {
		return apperr.New(apperr.CodeInvalidArgument, "CHECKPOINT_GRACE_MINUTES 必须在 0 到 240 之间")
	}
	if c.PermitUnitFeeCents <= 0 {
		return apperr.New(apperr.CodeInvalidArgument, "PERMIT_UNIT_FEE_CENTS 必须为正数")
	}
	if c.IdempotencyTTL < time.Minute {
		return apperr.New(apperr.CodeInvalidArgument, "IDEMPOTENCY_TTL 不能小于 1 分钟")
	}
	if c.PasswordKDFIterations < 10000 {
		return apperr.New(apperr.CodeInvalidArgument, "密码派生迭代次数过低")
	}
	return nil
}

// Redacted renders the configuration for startup logs without secrets.
func (c Config) Redacted() string {
	return fmt.Sprintf(
		"addr=%s dsn=%s log=%s session_ttl=%s workers=%d lease=%s max_attempts=%d grace_min=%d unit_fee=%d seed=%t",
		c.HTTPAddr, c.DatabaseDSN, c.LogLevel, c.SessionTTL, c.WorkerCount,
		c.JobLeaseDuration, c.JobMaxAttempts, c.CheckpointGraceMin, c.PermitUnitFeeCents, c.SeedEnabled,
	)
}

func envString(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok && strings.TrimSpace(value) != "" {
		return strings.TrimSpace(value)
	}
	return fallback
}

func envInt(key string, fallback int) (int, error) {
	value, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(value) == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return 0, apperr.Wrapf(apperr.CodeInvalidArgument, err, "%s 必须是整数", key)
	}
	return parsed, nil
}

func envBool(key string, fallback bool) (bool, error) {
	value, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(value) == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseBool(strings.TrimSpace(value))
	if err != nil {
		return false, apperr.Wrapf(apperr.CodeInvalidArgument, err, "%s 必须是布尔值", key)
	}
	return parsed, nil
}

func envDuration(key string, fallback time.Duration) (time.Duration, error) {
	value, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(value) == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(strings.TrimSpace(value))
	if err != nil {
		return 0, apperr.Wrapf(apperr.CodeInvalidArgument, err, "%s 必须是合法时长，例如 30s 或 12h", key)
	}
	if parsed <= 0 {
		return 0, apperr.Newf(apperr.CodeInvalidArgument, "%s 必须为正数", key)
	}
	return parsed, nil
}
