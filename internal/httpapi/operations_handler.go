package httpapi

import (
	"net/http"

	"github.com/vance1852/trail-permit-dispatch/internal/domain"
	"github.com/vance1852/trail-permit-dispatch/internal/httpjson"
	"github.com/vance1852/trail-permit-dispatch/internal/repository/sqliterepo"
	"github.com/vance1852/trail-permit-dispatch/internal/service/catalog"
	"github.com/vance1852/trail-permit-dispatch/internal/service/incident"
)

// createTrailRequest is the body of the trail registration endpoint.
type createTrailRequest struct {
	Code              string                    `json:"code"`
	Name              string                    `json:"name"`
	Region            string                    `json:"region"`
	Difficulty        int                       `json:"difficulty"`
	DistanceKM        float64                   `json:"distance_km"`
	DailyQuota        int                       `json:"daily_quota"`
	MinPartySize      int                       `json:"min_party_size"`
	MaxPartySize      int                       `json:"max_party_size"`
	PermitCutoffHours int                       `json:"permit_cutoff_hours"`
	Checkpoints       []catalog.CheckpointInput `json:"checkpoints"`
}

// handleCreateTrail registers a governed route.
func (s *Server) handleCreateTrail(w http.ResponseWriter, r *http.Request) {
	actor, err := httpjson.RequireActor(r.Context())
	if err != nil {
		httpjson.WriteError(w, r, err)
		return
	}
	var body createTrailRequest
	if err := httpjson.Decode(r, &body); err != nil {
		httpjson.WriteError(w, r, err)
		return
	}
	view, err := s.deps.Catalog.CreateTrail(r.Context(), actor, catalog.CreateTrailInput{
		Code:              body.Code,
		Name:              body.Name,
		Region:            body.Region,
		Difficulty:        body.Difficulty,
		DistanceKM:        body.DistanceKM,
		DailyQuota:        body.DailyQuota,
		MinPartySize:      body.MinPartySize,
		MaxPartySize:      body.MaxPartySize,
		PermitCutoffHours: body.PermitCutoffHours,
		Checkpoints:       body.Checkpoints,
	})
	if err != nil {
		httpjson.WriteError(w, r, err)
		return
	}
	httpjson.WriteData(w, r, http.StatusCreated, view)
}

// trailStatusRequest is the body of the trail status endpoint.
type trailStatusRequest struct {
	Status string `json:"status"`
}

// handleTrailStatus opens, closes or suspends a route.
func (s *Server) handleTrailStatus(w http.ResponseWriter, r *http.Request) {
	actor, err := httpjson.RequireActor(r.Context())
	if err != nil {
		httpjson.WriteError(w, r, err)
		return
	}
	var body trailStatusRequest
	if err := httpjson.Decode(r, &body); err != nil {
		httpjson.WriteError(w, r, err)
		return
	}
	view, err := s.deps.Catalog.SetTrailStatus(r.Context(), actor, r.PathValue("code"), body.Status)
	if err != nil {
		httpjson.WriteError(w, r, err)
		return
	}
	httpjson.WriteData(w, r, http.StatusOK, view)
}

// handleAddCheckpoint appends a reporting node to a route.
func (s *Server) handleAddCheckpoint(w http.ResponseWriter, r *http.Request) {
	actor, err := httpjson.RequireActor(r.Context())
	if err != nil {
		httpjson.WriteError(w, r, err)
		return
	}
	var body catalog.CheckpointInput
	if err := httpjson.Decode(r, &body); err != nil {
		httpjson.WriteError(w, r, err)
		return
	}
	view, err := s.deps.Catalog.AddCheckpoint(r.Context(), actor, r.PathValue("code"), body)
	if err != nil {
		httpjson.WriteError(w, r, err)
		return
	}
	httpjson.WriteData(w, r, http.StatusCreated, view)
}

// openWindowRequest is the body of the permit window endpoint.
type openWindowRequest struct {
	HikeDay    string `json:"hike_day"`
	QuotaTotal int    `json:"quota_total"`
}

// handleOpenWindow opens the permit window of a trail day.
func (s *Server) handleOpenWindow(w http.ResponseWriter, r *http.Request) {
	actor, err := httpjson.RequireActor(r.Context())
	if err != nil {
		httpjson.WriteError(w, r, err)
		return
	}
	var body openWindowRequest
	if err := httpjson.Decode(r, &body); err != nil {
		httpjson.WriteError(w, r, err)
		return
	}
	view, err := s.deps.Catalog.OpenWindow(r.Context(), actor, r.PathValue("code"), body.HikeDay, body.QuotaTotal)
	if err != nil {
		httpjson.WriteError(w, r, err)
		return
	}
	httpjson.WriteData(w, r, http.StatusCreated, view)
}

// handleCloseWindow closes the permit window of a trail day.
func (s *Server) handleCloseWindow(w http.ResponseWriter, r *http.Request) {
	actor, err := httpjson.RequireActor(r.Context())
	if err != nil {
		httpjson.WriteError(w, r, err)
		return
	}
	view, err := s.deps.Catalog.CloseWindow(r.Context(), actor, r.PathValue("code"), r.PathValue("day"))
	if err != nil {
		httpjson.WriteError(w, r, err)
		return
	}
	httpjson.WriteData(w, r, http.StatusOK, view)
}

// handleListWindows lists the permit windows of a trail.
func (s *Server) handleListWindows(w http.ResponseWriter, r *http.Request) {
	views, err := s.deps.Catalog.ListWindows(r.Context(), r.PathValue("code"),
		r.URL.Query().Get("from"), r.URL.Query().Get("to"))
	if err != nil {
		httpjson.WriteError(w, r, err)
		return
	}
	httpjson.WriteData(w, r, http.StatusOK, map[string]any{"items": views})
}

// handleListTrails returns a filtered page of trails.
func (s *Server) handleListTrails(w http.ResponseWriter, r *http.Request) {
	page, err := httpjson.PageRequest(r, sqliterepo.TrailSortKeys())
	if err != nil {
		httpjson.WriteError(w, r, err)
		return
	}
	result, err := s.deps.Catalog.ListTrails(r.Context(), r.URL.Query().Get("region"), r.URL.Query().Get("status"), page)
	if err != nil {
		httpjson.WriteError(w, r, err)
		return
	}
	httpjson.WriteData(w, r, http.StatusOK, result)
}

// handleGetTrail returns one trail with its checkpoints.
func (s *Server) handleGetTrail(w http.ResponseWriter, r *http.Request) {
	view, err := s.deps.Catalog.GetTrail(r.Context(), r.PathValue("code"))
	if err != nil {
		httpjson.WriteError(w, r, err)
		return
	}
	httpjson.WriteData(w, r, http.StatusOK, view)
}

// openIncidentRequest is the body of the incident reporting endpoint.
type openIncidentRequest struct {
	PartyCode     string `json:"party_code"`
	Kind          string `json:"kind"`
	Severity      string `json:"severity"`
	Summary       string `json:"summary"`
	CheckpointSeq int    `json:"checkpoint_seq"`
}

// handleOpenIncident reports a trail safety event.
func (s *Server) handleOpenIncident(w http.ResponseWriter, r *http.Request) {
	actor, err := httpjson.RequireActor(r.Context())
	if err != nil {
		httpjson.WriteError(w, r, err)
		return
	}
	var body openIncidentRequest
	if err := httpjson.Decode(r, &body); err != nil {
		httpjson.WriteError(w, r, err)
		return
	}
	view, err := s.deps.Incidents.Open(r.Context(), actor, incident.OpenInput{
		PartyCode:     body.PartyCode,
		Kind:          body.Kind,
		Severity:      body.Severity,
		Summary:       body.Summary,
		CheckpointSeq: body.CheckpointSeq,
	})
	if err != nil {
		httpjson.WriteError(w, r, err)
		return
	}
	httpjson.WriteData(w, r, http.StatusCreated, view)
}

// resolveIncidentRequest is the body of the incident resolution endpoint.
type resolveIncidentRequest struct {
	Resolution string `json:"resolution"`
}

// handleResolveIncident records the operator resolution of an incident.
func (s *Server) handleResolveIncident(w http.ResponseWriter, r *http.Request) {
	actor, err := httpjson.RequireActor(r.Context())
	if err != nil {
		httpjson.WriteError(w, r, err)
		return
	}
	id, err := httpjson.QueryInt64(r.PathValue("id"), "id")
	if err != nil {
		httpjson.WriteError(w, r, err)
		return
	}
	var body resolveIncidentRequest
	if err := httpjson.Decode(r, &body); err != nil {
		httpjson.WriteError(w, r, err)
		return
	}
	view, err := s.deps.Incidents.Resolve(r.Context(), actor, id, body.Resolution)
	if err != nil {
		httpjson.WriteError(w, r, err)
		return
	}
	httpjson.WriteData(w, r, http.StatusOK, view)
}

// handleCloseIncident archives a resolved incident.
func (s *Server) handleCloseIncident(w http.ResponseWriter, r *http.Request) {
	actor, err := httpjson.RequireActor(r.Context())
	if err != nil {
		httpjson.WriteError(w, r, err)
		return
	}
	id, err := httpjson.QueryInt64(r.PathValue("id"), "id")
	if err != nil {
		httpjson.WriteError(w, r, err)
		return
	}
	view, err := s.deps.Incidents.Close(r.Context(), actor, id)
	if err != nil {
		httpjson.WriteError(w, r, err)
		return
	}
	httpjson.WriteData(w, r, http.StatusOK, view)
}

// handleListIncidents returns a filtered page of incidents.
func (s *Server) handleListIncidents(w http.ResponseWriter, r *http.Request) {
	actor, err := httpjson.RequireActor(r.Context())
	if err != nil {
		httpjson.WriteError(w, r, err)
		return
	}
	page, err := httpjson.PageRequest(r, sqliterepo.IncidentSortKeys())
	if err != nil {
		httpjson.WriteError(w, r, err)
		return
	}
	filter := domain.IncidentFilter{}
	for _, state := range httpjson.QueryCSV(r, "state") {
		filter.States = append(filter.States, domain.IncidentState(state))
	}
	for _, severity := range httpjson.QueryCSV(r, "severity") {
		filter.Severities = append(filter.Severities, domain.Severity(severity))
	}
	result, err := s.deps.Incidents.List(r.Context(), actor, filter, page, r.URL.Query().Get("party_code"))
	if err != nil {
		httpjson.WriteError(w, r, err)
		return
	}
	httpjson.WriteData(w, r, http.StatusOK, result)
}

// handleGetIncident returns one incident.
func (s *Server) handleGetIncident(w http.ResponseWriter, r *http.Request) {
	actor, err := httpjson.RequireActor(r.Context())
	if err != nil {
		httpjson.WriteError(w, r, err)
		return
	}
	id, err := httpjson.QueryInt64(r.PathValue("id"), "id")
	if err != nil {
		httpjson.WriteError(w, r, err)
		return
	}
	view, err := s.deps.Incidents.Get(r.Context(), actor, id)
	if err != nil {
		httpjson.WriteError(w, r, err)
		return
	}
	httpjson.WriteData(w, r, http.StatusOK, view)
}

// waiveSettlementRequest is the body of the fee waiver endpoint.
type waiveSettlementRequest struct {
	Note string `json:"note"`
}

// handleWaiveSettlement writes off a permit fee.
func (s *Server) handleWaiveSettlement(w http.ResponseWriter, r *http.Request) {
	actor, err := httpjson.RequireActor(r.Context())
	if err != nil {
		httpjson.WriteError(w, r, err)
		return
	}
	id, err := httpjson.QueryInt64(r.PathValue("id"), "id")
	if err != nil {
		httpjson.WriteError(w, r, err)
		return
	}
	var body waiveSettlementRequest
	if err := httpjson.Decode(r, &body); err != nil {
		httpjson.WriteError(w, r, err)
		return
	}
	view, err := s.deps.Settlements.Waive(r.Context(), actor, id, body.Note)
	if err != nil {
		httpjson.WriteError(w, r, err)
		return
	}
	httpjson.WriteData(w, r, http.StatusOK, view)
}

// handleListSettlements returns a filtered page of settlements.
func (s *Server) handleListSettlements(w http.ResponseWriter, r *http.Request) {
	actor, err := httpjson.RequireActor(r.Context())
	if err != nil {
		httpjson.WriteError(w, r, err)
		return
	}
	page, err := httpjson.PageRequest(r, sqliterepo.SettlementSortKeys())
	if err != nil {
		httpjson.WriteError(w, r, err)
		return
	}
	filter := domain.SettlementFilter{}
	for _, state := range httpjson.QueryCSV(r, "state") {
		filter.States = append(filter.States, domain.SettlementState(state))
	}
	result, err := s.deps.Settlements.List(r.Context(), actor, filter, page, r.URL.Query().Get("party_code"))
	if err != nil {
		httpjson.WriteError(w, r, err)
		return
	}
	httpjson.WriteData(w, r, http.StatusOK, result)
}

// handlePartySettlement returns the settlement of one party.
func (s *Server) handlePartySettlement(w http.ResponseWriter, r *http.Request) {
	actor, err := httpjson.RequireActor(r.Context())
	if err != nil {
		httpjson.WriteError(w, r, err)
		return
	}
	view, err := s.deps.Settlements.ForParty(r.Context(), actor, r.PathValue("code"))
	if err != nil {
		httpjson.WriteError(w, r, err)
		return
	}
	httpjson.WriteData(w, r, http.StatusOK, view)
}

// handleListAuditEvents returns a filtered page of audit events.
func (s *Server) handleListAuditEvents(w http.ResponseWriter, r *http.Request) {
	page, err := httpjson.PageRequest(r, sqliterepo.AuditSortKeys())
	if err != nil {
		httpjson.WriteError(w, r, err)
		return
	}
	actorID, err := httpjson.QueryInt(r, "actor_id", 0)
	if err != nil {
		httpjson.WriteError(w, r, err)
		return
	}
	filter := domain.AuditFilter{
		ActorID:    int64(actorID),
		Action:     r.URL.Query().Get("action"),
		ObjectType: r.URL.Query().Get("object_type"),
		ObjectID:   r.URL.Query().Get("object_id"),
		Result:     domain.AuditResult(r.URL.Query().Get("result")),
	}
	normalized, err := filter.Normalize()
	if err != nil {
		httpjson.WriteError(w, r, err)
		return
	}
	result, err := s.deps.AuditEvents.List(r.Context(), normalized, page)
	if err != nil {
		httpjson.WriteError(w, r, err)
		return
	}
	httpjson.WriteData(w, r, http.StatusOK, result)
}
