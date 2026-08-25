// Package httpapi exposes the platform over HTTP: routing, request parsing and
// the mapping of stable business errors onto status codes.
package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/vance1852/trail-permit-dispatch/internal/domain"
	"github.com/vance1852/trail-permit-dispatch/internal/middleware"
	"github.com/vance1852/trail-permit-dispatch/internal/repository"
	"github.com/vance1852/trail-permit-dispatch/internal/service/auth"
	"github.com/vance1852/trail-permit-dispatch/internal/service/catalog"
	"github.com/vance1852/trail-permit-dispatch/internal/service/dispatch"
	"github.com/vance1852/trail-permit-dispatch/internal/service/incident"
	"github.com/vance1852/trail-permit-dispatch/internal/service/settlement"
)

// HealthChecker reports the liveness and readiness of the process.
type HealthChecker interface {
	Live(ctx context.Context) error
	Ready(ctx context.Context) (ReadyReport, error)
}

// ReadyReport is the readiness payload of the platform.
type ReadyReport struct {
	Status        string         `json:"status"`
	SchemaVersion int            `json:"schema_version"`
	Trails        int            `json:"trails"`
	Rangers       int            `json:"rangers"`
	PendingJobs   map[string]int `json:"pending_jobs"`
	Pool          string         `json:"pool"`
}

// Deps carries the collaborators of the HTTP layer.
type Deps struct {
	Auth           *auth.Service
	Catalog        *catalog.Service
	Dispatch       *dispatch.Service
	Incidents      *incident.Service
	Settlements    *settlement.Service
	AuditEvents    repository.AuditRepository
	Health         HealthChecker
	Logger         *slog.Logger
	RequestTimeout time.Duration
}

// Server owns the routing table of the platform.
type Server struct {
	deps    Deps
	handler http.Handler
}

// New builds the HTTP server and wires the middleware pipeline.
func New(deps Deps) *Server {
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	if deps.RequestTimeout <= 0 {
		deps.RequestTimeout = 20 * time.Second
	}
	server := &Server{deps: deps}
	server.handler = middleware.Chain(server.routes(),
		middleware.RequestID(),
		middleware.AccessLog(deps.Logger),
		middleware.Recovery(deps.Logger),
		middleware.Timeout(deps.RequestTimeout),
	)
	return server
}

// Handler returns the fully decorated handler.
func (s *Server) Handler() http.Handler { return s.handler }

// ServeHTTP lets the server act as an http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handler.ServeHTTP(w, r)
}

// routes builds the routing table. Authentication and role checks are attached
// per route group so a new endpoint cannot accidentally be public.
func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	mux.Handle("GET /healthz", http.HandlerFunc(s.handleLive))
	mux.Handle("GET /readyz", http.HandlerFunc(s.handleReady))
	mux.Handle("POST /api/v1/auth/login", http.HandlerFunc(s.handleLogin))

	authenticated := func(handler http.HandlerFunc) http.Handler {
		return middleware.Chain(handler,
			middleware.Authenticate(s.deps.Auth, false),
			middleware.RequireAuthenticated(),
		)
	}
	leaderOnly := func(handler http.HandlerFunc) http.Handler {
		return middleware.Chain(handler,
			middleware.Authenticate(s.deps.Auth, false),
			middleware.RequireRole(domain.RoleLeader),
		)
	}
	rangerOnly := func(handler http.HandlerFunc) http.Handler {
		return middleware.Chain(handler,
			middleware.Authenticate(s.deps.Auth, false),
			middleware.RequireRole(domain.RoleRanger),
		)
	}

	mux.Handle("GET /api/v1/auth/me", authenticated(s.handleAccount))
	mux.Handle("POST /api/v1/auth/logout", authenticated(s.handleLogout))
	mux.Handle("POST /api/v1/auth/sessions/revoke-all", authenticated(s.handleRevokeAll))
	mux.Handle("POST /api/v1/accounts", rangerOnly(s.handleRegisterAccount))

	mux.Handle("GET /api/v1/trails", authenticated(s.handleListTrails))
	mux.Handle("GET /api/v1/trails/{code}", authenticated(s.handleGetTrail))
	mux.Handle("GET /api/v1/trails/{code}/permit-windows", authenticated(s.handleListWindows))
	mux.Handle("POST /api/v1/trails", rangerOnly(s.handleCreateTrail))
	mux.Handle("POST /api/v1/trails/{code}/status", rangerOnly(s.handleTrailStatus))
	mux.Handle("POST /api/v1/trails/{code}/checkpoints", rangerOnly(s.handleAddCheckpoint))
	mux.Handle("POST /api/v1/trails/{code}/permit-windows", rangerOnly(s.handleOpenWindow))
	mux.Handle("POST /api/v1/trails/{code}/permit-windows/{day}/close", rangerOnly(s.handleCloseWindow))

	mux.Handle("GET /api/v1/parties", authenticated(s.handleListParties))
	mux.Handle("GET /api/v1/parties/{code}", authenticated(s.handleGetParty))
	mux.Handle("POST /api/v1/parties", leaderOnly(s.handleCreateParty))
	mux.Handle("POST /api/v1/parties/{code}/members", leaderOnly(s.handleRegisterMembers))
	mux.Handle("POST /api/v1/parties/{code}/permit-request", leaderOnly(s.handleRequestPermit))
	mux.Handle("POST /api/v1/parties/{code}/checkpoints", leaderOnly(s.handleReportCheckpoint))
	mux.Handle("POST /api/v1/parties/{code}/complete", leaderOnly(s.handleCompleteParty))
	mux.Handle("POST /api/v1/parties/{code}/cancel", authenticated(s.handleCancelParty))
	mux.Handle("POST /api/v1/parties/{code}/approve", rangerOnly(s.handleApproveParty))
	mux.Handle("POST /api/v1/parties/{code}/dispatch", rangerOnly(s.handleDispatchParty))

	mux.Handle("GET /api/v1/incidents", authenticated(s.handleListIncidents))
	mux.Handle("GET /api/v1/incidents/{id}", authenticated(s.handleGetIncident))
	mux.Handle("POST /api/v1/incidents", authenticated(s.handleOpenIncident))
	mux.Handle("POST /api/v1/incidents/{id}/resolve", rangerOnly(s.handleResolveIncident))
	mux.Handle("POST /api/v1/incidents/{id}/close", rangerOnly(s.handleCloseIncident))

	mux.Handle("GET /api/v1/settlements", authenticated(s.handleListSettlements))
	mux.Handle("GET /api/v1/parties/{code}/settlement", authenticated(s.handlePartySettlement))
	mux.Handle("POST /api/v1/settlements/{id}/waive", rangerOnly(s.handleWaiveSettlement))

	mux.Handle("GET /api/v1/audit-events", rangerOnly(s.handleListAuditEvents))

	return mux
}
