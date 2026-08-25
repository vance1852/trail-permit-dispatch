// Command server runs the trail permit dispatch platform: HTTP API, background
// workers and the SQLite backed persistence layer.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/vance1852/trail-permit-dispatch/internal/bootstrap"
	"github.com/vance1852/trail-permit-dispatch/internal/clock"
	"github.com/vance1852/trail-permit-dispatch/internal/config"
	"github.com/vance1852/trail-permit-dispatch/internal/logging"
)

func main() {
	if err := run(); err != nil {
		slog.Default().Error("服务启动失败", slog.String("error", err.Error()))
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	logger := logging.New(cfg.LogLevel)
	slog.SetDefault(logger)
	logger.Info("加载配置完成", slog.String("config", cfg.Redacted()))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	app, err := bootstrap.New(ctx, cfg, logger, clock.System{})
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := app.Close(); closeErr != nil {
			logger.Error("释放资源失败", slog.String("error", closeErr.Error()))
		}
	}()

	if cfg.SeedEnabled {
		seeded, err := app.Seed(ctx)
		if err != nil {
			return err
		}
		if seeded.Skipped {
			logger.Info("检测到已有数据，跳过初始化")
		}
	}
	if err := app.StartWorker(ctx); err != nil {
		return err
	}
	return app.Serve(ctx)
}
