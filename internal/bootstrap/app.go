// Package bootstrap wires the configuration, database, repositories, services,
// worker and HTTP server into one runnable application.
package bootstrap

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/vance1852/trail-permit-dispatch/internal/apperr"
	"github.com/vance1852/trail-permit-dispatch/internal/audit"
	"github.com/vance1852/trail-permit-dispatch/internal/clock"
	"github.com/vance1852/trail-permit-dispatch/internal/config"
	"github.com/vance1852/trail-permit-dispatch/internal/domain"
	"github.com/vance1852/trail-permit-dispatch/internal/httpapi"
	"github.com/vance1852/trail-permit-dispatch/internal/idempotency"
	"github.com/vance1852/trail-permit-dispatch/internal/repository/sqliterepo"
	"github.com/vance1852/trail-permit-dispatch/internal/security"
	"github.com/vance1852/trail-permit-dispatch/internal/service/auth"
	"github.com/vance1852/trail-permit-dispatch/internal/service/catalog"
	"github.com/vance1852/trail-permit-dispatch/internal/service/dispatch"
	"github.com/vance1852/trail-permit-dispatch/internal/service/incident"
	"github.com/vance1852/trail-permit-dispatch/internal/service/settlement"
	"github.com/vance1852/trail-permit-dispatch/internal/storage/sqlite"
	"github.com/vance1852/trail-permit-dispatch/internal/worker"
)

// App holds every long lived component of the process.
type App struct {
	Config      config.Config
	Logger      *slog.Logger
	DB          *sql.DB
	Tx          *sqlite.TxManager
	Clock       clock.Clock
	Auth        *auth.Service
	Catalog     *catalog.Service
	Dispatch    *dispatch.Service
	Incidents   *incident.Service
	Settlements *settlement.Service
	Worker      *worker.Runner
	Jobs        *sqliterepo.JobRepo
	Audits      *sqliterepo.AuditRepo
	Users       *sqliterepo.UserRepo
	Trails      *sqliterepo.TrailRepo
	HTTP        *httpapi.Server

	workerServices worker.Services
	schemaVersion  int
}

// New builds the application graph and migrates the database.
func New(ctx context.Context, cfg config.Config, logger *slog.Logger, clk clock.Clock) (*App, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if clk == nil {
		clk = clock.System{}
	}
	db, err := sqlite.Open(ctx, cfg.DatabaseDSN)
	if err != nil {
		return nil, err
	}
	result, err := sqlite.Migrate(ctx, db)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	logger.Info("数据库迁移完成",
		slog.Int("schema_version", result.Version),
		slog.Bool("already_latest", result.AlreadyLatest),
		slog.Any("applied", result.Applied),
	)

	app, err := Assemble(cfg, logger, clk, db)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	app.schemaVersion = result.Version
	return app, nil
}

// Assemble wires the graph on an already migrated handle. Tests use it directly.
func Assemble(cfg config.Config, logger *slog.Logger, clk clock.Clock, db *sql.DB) (*App, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if logger == nil {
		logger = slog.Default()
	}
	if clk == nil {
		clk = clock.System{}
	}
	tx := sqlite.NewTxManager(db)

	users := sqliterepo.NewUserRepo(tx)
	sessions := sqliterepo.NewSessionRepo(tx)
	trails := sqliterepo.NewTrailRepo(tx)
	permits := sqliterepo.NewPermitRepo(tx)
	parties := sqliterepo.NewPartyRepo(tx)
	reports := sqliterepo.NewCheckpointReportRepo(tx)
	incidents := sqliterepo.NewIncidentRepo(tx)
	settlements := sqliterepo.NewSettlementRepo(tx)
	audits := sqliterepo.NewAuditRepo(tx)
	jobs := sqliterepo.NewJobRepo(tx)
	idemRecords := sqliterepo.NewIdempotencyRepo(tx)

	recorder := audit.New(audits, clk)
	idem := idempotency.New(idemRecords, clk, cfg.IdempotencyTTL)

	authService := auth.New(auth.Deps{
		Tx:         tx,
		Users:      users,
		Sessions:   sessions,
		Audit:      recorder,
		Hasher:     security.NewHasher(cfg.PasswordKDFIterations),
		Clock:      clk,
		SessionTTL: cfg.SessionTTL,
	})
	catalogService := catalog.New(catalog.Deps{
		Tx:      tx,
		Trails:  trails,
		Permits: permits,
		Audit:   recorder,
		Clock:   clk,
	})
	dispatchService := dispatch.New(dispatch.Deps{
		Tx:          tx,
		Parties:     parties,
		Trails:      trails,
		Permits:     permits,
		Reports:     reports,
		Settlements: settlements,
		Jobs:        jobs,
		Audit:       recorder,
		Idempotency: idem,
		Clock:       clk,
		Options: dispatch.Options{
			CheckpointGraceMinutes: cfg.CheckpointGraceMin,
			PermitUnitFeeCents:     cfg.PermitUnitFeeCents,
			JobMaxAttempts:         cfg.JobMaxAttempts,
		},
	})
	incidentService := incident.New(incident.Deps{
		Tx:             tx,
		Incidents:      incidents,
		Parties:        parties,
		Jobs:           jobs,
		Audit:          recorder,
		Clock:          clk,
		JobMaxAttempts: cfg.JobMaxAttempts,
	})
	settlementService := settlement.New(settlement.Deps{
		Tx:          tx,
		Settlements: settlements,
		Parties:     parties,
		Audit:       recorder,
		Clock:       clk,
	})

	// The two operational services reference each other through narrow
	// interfaces, so the wiring is completed here instead of in a constructor.
	dispatchService.AttachIncidentOpener(incidentService)
	incidentService.AttachPartyAborter(dispatchService)

	runner := worker.New(worker.Deps{
		Jobs:   jobs,
		Clock:  clk,
		Logger: logger,
		Options: worker.Options{
			WorkerCount:  cfg.WorkerCount,
			PollInterval: cfg.WorkerPollInterval,
			Lease:        cfg.JobLeaseDuration,
			JobTimeout:   cfg.WorkerJobTimeout,
			BackoffBase:  cfg.JobBackoffBase,
			BackoffMax:   cfg.JobBackoffMax,
		},
	})
	workerServices := worker.Services{
		Dispatch:       dispatchService,
		Incidents:      incidentService,
		Settlements:    settlementService,
		Auth:           authService,
		Jobs:           jobs,
		Tx:             tx,
		Audit:          recorder,
		Clock:          clk,
		Logger:         logger,
		MaxAttempts:    cfg.JobMaxAttempts,
		SweepInterval:  time.Minute,
		ExpiryInterval: 10 * time.Minute,
	}
	worker.Register(runner, workerServices)

	app := &App{
		Config:         cfg,
		Logger:         logger,
		DB:             db,
		Tx:             tx,
		Clock:          clk,
		Auth:           authService,
		Catalog:        catalogService,
		Dispatch:       dispatchService,
		Incidents:      incidentService,
		Settlements:    settlementService,
		Worker:         runner,
		Jobs:           jobs,
		Audits:         audits,
		Users:          users,
		Trails:         trails,
		workerServices: workerServices,
	}
	app.HTTP = httpapi.New(httpapi.Deps{
		Auth:           authService,
		Catalog:        catalogService,
		Dispatch:       dispatchService,
		Incidents:      incidentService,
		Settlements:    settlementService,
		AuditEvents:    audits,
		Health:         app,
		Logger:         logger,
		RequestTimeout: cfg.WorkerJobTimeout,
	})
	return app, nil
}

// Live implements the liveness probe.
func (a *App) Live(ctx context.Context) error {
	if err := a.DB.PingContext(ctx); err != nil {
		return apperr.Wrap(apperr.CodeUnavailable, "数据库连接不可用", err)
	}
	return nil
}

// Ready implements the readiness probe. It verifies the schema version, the seed
// data required to operate and the background queue depth.
func (a *App) Ready(ctx context.Context) (httpapi.ReadyReport, error) {
	if err := a.Live(ctx); err != nil {
		return httpapi.ReadyReport{}, err
	}
	if err := sqlite.VerifySchema(ctx, a.DB); err != nil {
		return httpapi.ReadyReport{}, err
	}
	version, err := sqlite.SchemaVersion(ctx, a.DB)
	if err != nil {
		return httpapi.ReadyReport{}, err
	}
	rangers, err := a.Auth.CountRole(ctx, domain.RoleRanger)
	if err != nil {
		return httpapi.ReadyReport{}, err
	}
	if rangers == 0 {
		return httpapi.ReadyReport{}, apperr.New(apperr.CodeUnavailable, "尚未配置线路管理员账号")
	}
	trails, err := a.Catalog.TrailCount(ctx)
	if err != nil {
		return httpapi.ReadyReport{}, err
	}
	pending, err := a.Jobs.PendingKinds(ctx)
	if err != nil {
		return httpapi.ReadyReport{}, err
	}
	summary := make(map[string]int, len(pending))
	for kind, total := range pending {
		summary[string(kind)] = total
	}
	return httpapi.ReadyReport{
		Status:        "ready",
		SchemaVersion: version,
		Trails:        trails,
		Rangers:       rangers,
		PendingJobs:   summary,
		Pool:          a.Tx.Stats(),
	}, nil
}

// StartWorker reclaims stale leases, ensures the recurring jobs exist and starts
// the background workers.
func (a *App) StartWorker(ctx context.Context) error {
	if err := a.workerServices.EnsureRecurring(ctx); err != nil {
		return err
	}
	return a.Worker.Start(ctx)
}

// Close stops the worker and releases the database handle.
func (a *App) Close() error {
	a.Worker.Stop()
	return a.DB.Close()
}

// Serve runs the HTTP server until the context is cancelled and then shuts down
// gracefully within the configured timeout.
func (a *App) Serve(ctx context.Context) error {
	server := &http.Server{
		Addr:              a.Config.HTTPAddr,
		Handler:           a.HTTP.Handler(),
		ReadHeaderTimeout: a.Config.ReadHeaderTimeout,
	}
	errs := make(chan error, 1)
	go func() {
		a.Logger.Info("HTTP 服务启动", slog.String("addr", a.Config.HTTPAddr))
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- err
			return
		}
		errs <- nil
	}()

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), a.Config.ShutdownTimeout)
		defer cancel()
		a.Logger.Info("收到停止信号，开始优雅关闭")
		if err := server.Shutdown(shutdownCtx); err != nil {
			return apperr.Wrap(apperr.CodeUnavailable, "HTTP 优雅关闭失败", err)
		}
		return <-errs
	}
}
