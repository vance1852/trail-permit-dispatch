package config

import (
	"strings"
	"testing"
	"time"

	"github.com/vance1852/trail-permit-dispatch/internal/apperr"
)

func TestLoadUsesDefaultsWhenEnvironmentIsEmpty(t *testing.T) {
	cfg, err := Load()
	if err != nil {
		t.Fatalf("默认配置应可加载: %v", err)
	}
	if cfg.HTTPAddr != ":8080" {
		t.Fatalf("默认监听地址错误: %q", cfg.HTTPAddr)
	}
	if cfg.SessionTTL != 12*time.Hour || cfg.WorkerCount != 2 {
		t.Fatalf("默认时长或并发错误: %+v", cfg)
	}
	if cfg.PermitUnitFeeCents != 4500 || cfg.CheckpointGraceMin != 30 {
		t.Fatalf("默认业务参数错误: %+v", cfg)
	}
	if !cfg.SeedEnabled {
		t.Fatal("默认应允许初始化基础数据")
	}
	if cfg.SeedRangerPassword != "" || cfg.SeedLeaderPassword != "" {
		t.Fatal("初始口令不得存在默认值，必须由环境注入或由系统一次性生成")
	}
}

func TestSeedPasswordsComeOnlyFromEnvironment(t *testing.T) {
	t.Setenv("SEED_RANGER_PASSWORD", "injected-ranger-2026")
	t.Setenv("SEED_LEADER_PASSWORD", "injected-leader-2026")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}
	if cfg.SeedRangerPassword != "injected-ranger-2026" || cfg.SeedLeaderPassword != "injected-leader-2026" {
		t.Fatalf("注入的初始口令未生效: %+v", cfg.Redacted())
	}
}

func TestLoadParsesEnvironmentOverrides(t *testing.T) {
	t.Setenv("HTTP_ADDR", "127.0.0.1:9090")
	t.Setenv("SESSION_TTL", "45m")
	t.Setenv("WORKER_COUNT", "4")
	t.Setenv("JOB_MAX_ATTEMPTS", "7")
	t.Setenv("PERMIT_UNIT_FEE_CENTS", "6000")
	t.Setenv("SEED_ENABLED", "false")
	t.Setenv("LOG_LEVEL", "DEBUG")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}
	if cfg.HTTPAddr != "127.0.0.1:9090" || cfg.SessionTTL != 45*time.Minute {
		t.Fatalf("环境变量未生效: %+v", cfg)
	}
	if cfg.WorkerCount != 4 || cfg.JobMaxAttempts != 7 || cfg.PermitUnitFeeCents != 6000 {
		t.Fatalf("数值型配置未生效: %+v", cfg)
	}
	if cfg.SeedEnabled {
		t.Fatal("SEED_ENABLED=false 应关闭初始化")
	}
	if cfg.LogLevel != "debug" {
		t.Fatalf("日志级别应归一化为小写: %q", cfg.LogLevel)
	}
}

func TestLoadRejectsMalformedValues(t *testing.T) {
	t.Setenv("SESSION_TTL", "half-an-hour")
	if _, err := Load(); !apperr.Is(err, apperr.CodeInvalidArgument) {
		t.Fatalf("非法时长应被拒绝，实际 %v", err)
	}
	t.Setenv("SESSION_TTL", "-5m")
	if _, err := Load(); !apperr.Is(err, apperr.CodeInvalidArgument) {
		t.Fatalf("负时长应被拒绝，实际 %v", err)
	}
	t.Setenv("SESSION_TTL", "1h")
	t.Setenv("WORKER_COUNT", "many")
	if _, err := Load(); !apperr.Is(err, apperr.CodeInvalidArgument) {
		t.Fatalf("非整数并发数应被拒绝，实际 %v", err)
	}
	t.Setenv("WORKER_COUNT", "2")
	t.Setenv("SEED_ENABLED", "perhaps")
	if _, err := Load(); !apperr.Is(err, apperr.CodeInvalidArgument) {
		t.Fatalf("非布尔值应被拒绝，实际 %v", err)
	}
	t.Setenv("SEED_ENABLED", "true")
	t.Setenv("LOG_LEVEL", "verbose")
	if _, err := Load(); !apperr.Is(err, apperr.CodeInvalidArgument) {
		t.Fatalf("未知日志级别应被拒绝，实际 %v", err)
	}
}

// validConfig returns a configuration that satisfies every rule.
func validConfig() Config {
	return Config{
		HTTPAddr:              ":8080",
		DatabaseDSN:           "file:data/trail.sqlite",
		LogLevel:              "info",
		SessionTTL:            time.Hour,
		ShutdownTimeout:       5 * time.Second,
		ReadHeaderTimeout:     5 * time.Second,
		WorkerCount:           2,
		WorkerPollInterval:    500 * time.Millisecond,
		WorkerJobTimeout:      10 * time.Second,
		JobLeaseDuration:      30 * time.Second,
		JobBackoffBase:        time.Second,
		JobBackoffMax:         time.Minute,
		JobMaxAttempts:        5,
		CheckpointGraceMin:    30,
		PermitUnitFeeCents:    4500,
		IdempotencyTTL:        time.Hour,
		PasswordKDFIterations: 120000,
	}
}

func TestValidateRejectsInconsistentConfigurations(t *testing.T) {
	if err := validConfig().Validate(); err != nil {
		t.Fatalf("合法配置被拒绝: %v", err)
	}

	cases := map[string]func(*Config){
		"空监听地址":      func(c *Config) { c.HTTPAddr = "  " },
		"空数据库 DSN":   func(c *Config) { c.DatabaseDSN = "" },
		"会话有效期过短":    func(c *Config) { c.SessionTTL = time.Second },
		"并发数超出范围":    func(c *Config) { c.WorkerCount = 64 },
		"轮询间隔过短":     func(c *Config) { c.WorkerPollInterval = time.Millisecond },
		"租约不大于轮询间隔":  func(c *Config) { c.JobLeaseDuration = c.WorkerPollInterval },
		"重试次数越界":     func(c *Config) { c.JobMaxAttempts = 0 },
		"退避区间无效":     func(c *Config) { c.JobBackoffMax = time.Millisecond },
		"宽限期越界":      func(c *Config) { c.CheckpointGraceMin = 999 },
		"许可单价非正":     func(c *Config) { c.PermitUnitFeeCents = 0 },
		"幂等保留期过短":    func(c *Config) { c.IdempotencyTTL = time.Second },
		"密码派生迭代次数过低": func(c *Config) { c.PasswordKDFIterations = 100 },
	}
	for name, mutate := range cases {
		cfg := validConfig()
		mutate(&cfg)
		if err := cfg.Validate(); err == nil {
			t.Fatalf("%s 的配置应被拒绝", name)
		}
	}
}

func TestRedactedOmitsSecrets(t *testing.T) {
	cfg := validConfig()
	cfg.SeedRangerPassword = "super-secret-2026"
	cfg.SeedLeaderPassword = "another-secret-2026"
	rendered := cfg.Redacted()
	if strings.Contains(rendered, "super-secret-2026") || strings.Contains(rendered, "another-secret-2026") {
		t.Fatalf("配置摘要不得包含密码: %s", rendered)
	}
	if !strings.Contains(rendered, "addr=:8080") || !strings.Contains(rendered, "workers=2") {
		t.Fatalf("配置摘要缺少关键运行参数: %s", rendered)
	}
}
