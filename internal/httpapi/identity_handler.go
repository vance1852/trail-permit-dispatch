package httpapi

import (
	"net/http"

	"github.com/vance1852/trail-permit-dispatch/internal/httpjson"
	"github.com/vance1852/trail-permit-dispatch/internal/service/auth"
)

// loginRequest is the body of the login endpoint.
type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// handleLogin authenticates an account and returns a bearer token.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var body loginRequest
	if err := httpjson.Decode(r, &body); err != nil {
		httpjson.WriteError(w, r, err)
		return
	}
	result, err := s.deps.Auth.Login(r.Context(), body.Email, body.Password, r.UserAgent())
	if err != nil {
		httpjson.WriteError(w, r, err)
		return
	}
	httpjson.WriteData(w, r, http.StatusOK, result)
}

// handleAccount returns the account of the current session.
func (s *Server) handleAccount(w http.ResponseWriter, r *http.Request) {
	actor, err := httpjson.RequireActor(r.Context())
	if err != nil {
		httpjson.WriteError(w, r, err)
		return
	}
	account, err := s.deps.Auth.Account(r.Context(), actor)
	if err != nil {
		httpjson.WriteError(w, r, err)
		return
	}
	active, err := s.deps.Auth.ActiveSessions(r.Context(), actor)
	if err != nil {
		httpjson.WriteError(w, r, err)
		return
	}
	httpjson.WriteData(w, r, http.StatusOK, map[string]any{
		"account":         account,
		"session_id":      actor.SessionID,
		"active_sessions": active,
	})
}

// handleLogout revokes the session of the current request.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	actor, err := httpjson.RequireActor(r.Context())
	if err != nil {
		httpjson.WriteError(w, r, err)
		return
	}
	if err := s.deps.Auth.Logout(r.Context(), actor); err != nil {
		httpjson.WriteError(w, r, err)
		return
	}
	httpjson.WriteData(w, r, http.StatusOK, map[string]any{"revoked": true})
}

// handleRevokeAll revokes every session of the current account.
func (s *Server) handleRevokeAll(w http.ResponseWriter, r *http.Request) {
	actor, err := httpjson.RequireActor(r.Context())
	if err != nil {
		httpjson.WriteError(w, r, err)
		return
	}
	revoked, err := s.deps.Auth.RevokeAll(r.Context(), actor)
	if err != nil {
		httpjson.WriteError(w, r, err)
		return
	}
	httpjson.WriteData(w, r, http.StatusOK, map[string]any{"revoked": revoked})
}

// registerAccountRequest is the body of the account creation endpoint.
type registerAccountRequest struct {
	Email       string `json:"email"`
	DisplayName string `json:"display_name"`
	Password    string `json:"password"`
	Role        string `json:"role"`
}

// handleRegisterAccount lets a ranger create leader or ranger accounts.
func (s *Server) handleRegisterAccount(w http.ResponseWriter, r *http.Request) {
	actor, err := httpjson.RequireActor(r.Context())
	if err != nil {
		httpjson.WriteError(w, r, err)
		return
	}
	var body registerAccountRequest
	if err := httpjson.Decode(r, &body); err != nil {
		httpjson.WriteError(w, r, err)
		return
	}
	account, err := s.deps.Auth.Register(r.Context(), actor, auth.RegisterInput{
		Email:       body.Email,
		DisplayName: body.DisplayName,
		Password:    body.Password,
		Role:        body.Role,
	})
	if err != nil {
		httpjson.WriteError(w, r, err)
		return
	}
	httpjson.WriteData(w, r, http.StatusCreated, account)
}

// handleLive answers the liveness probe.
func (s *Server) handleLive(w http.ResponseWriter, r *http.Request) {
	if err := s.deps.Health.Live(r.Context()); err != nil {
		httpjson.WriteError(w, r, err)
		return
	}
	httpjson.WriteData(w, r, http.StatusOK, map[string]string{"status": "alive"})
}

// handleReady answers the readiness probe including dependency checks.
func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	report, err := s.deps.Health.Ready(r.Context())
	if err != nil {
		httpjson.WriteError(w, r, err)
		return
	}
	httpjson.WriteData(w, r, http.StatusOK, report)
}
