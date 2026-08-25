package httpapi

import (
	"net/http"
	"time"

	"github.com/vance1852/trail-permit-dispatch/internal/apperr"
	"github.com/vance1852/trail-permit-dispatch/internal/domain"
	"github.com/vance1852/trail-permit-dispatch/internal/httpjson"
	"github.com/vance1852/trail-permit-dispatch/internal/repository/sqliterepo"
	"github.com/vance1852/trail-permit-dispatch/internal/service/dispatch"
)

// createPartyRequest is the body of the party creation endpoint.
type createPartyRequest struct {
	TrailCode    string `json:"trail_code"`
	HikeDay      string `json:"hike_day"`
	PlannedStart string `json:"planned_start"`
	PlannedEnd   string `json:"planned_end"`
	Size         int    `json:"size"`
	ContactPhone string `json:"contact_phone"`
	Notes        string `json:"notes"`
	LeaderRef    string `json:"leader_ref"`
}

// handleCreateParty registers a new party plan.
func (s *Server) handleCreateParty(w http.ResponseWriter, r *http.Request) {
	actor, err := httpjson.RequireActor(r.Context())
	if err != nil {
		httpjson.WriteError(w, r, err)
		return
	}
	var body createPartyRequest
	if err := httpjson.Decode(r, &body); err != nil {
		httpjson.WriteError(w, r, err)
		return
	}
	plannedStart, err := parseTimestamp(body.PlannedStart, "planned_start")
	if err != nil {
		httpjson.WriteError(w, r, err)
		return
	}
	plannedEnd, err := parseTimestamp(body.PlannedEnd, "planned_end")
	if err != nil {
		httpjson.WriteError(w, r, err)
		return
	}
	view, err := s.deps.Dispatch.CreateParty(r.Context(), actor, dispatch.CreatePartyInput{
		TrailCode:    body.TrailCode,
		HikeDay:      body.HikeDay,
		PlannedStart: plannedStart,
		PlannedEnd:   plannedEnd,
		Size:         body.Size,
		ContactPhone: body.ContactPhone,
		Notes:        body.Notes,
		LeaderRef:    body.LeaderRef,
	})
	if err != nil {
		httpjson.WriteError(w, r, err)
		return
	}
	httpjson.WriteData(w, r, http.StatusCreated, view)
}

// registerMembersRequest is the body of the batch member endpoint.
type registerMembersRequest struct {
	Members []dispatch.MemberInput `json:"members"`
}

// handleRegisterMembers registers hikers on a draft party.
func (s *Server) handleRegisterMembers(w http.ResponseWriter, r *http.Request) {
	actor, err := httpjson.RequireActor(r.Context())
	if err != nil {
		httpjson.WriteError(w, r, err)
		return
	}
	var body registerMembersRequest
	if err := httpjson.Decode(r, &body); err != nil {
		httpjson.WriteError(w, r, err)
		return
	}
	result, err := s.deps.Dispatch.RegisterMembers(r.Context(), actor, r.PathValue("code"), body.Members)
	if err != nil {
		httpjson.WriteError(w, r, err)
		return
	}
	status := http.StatusOK
	if result.Accepted == 0 {
		status = http.StatusConflict
	}
	httpjson.WriteData(w, r, status, result)
}

// handleRequestPermit reserves permit seats for a ready party.
func (s *Server) handleRequestPermit(w http.ResponseWriter, r *http.Request) {
	actor, err := httpjson.RequireActor(r.Context())
	if err != nil {
		httpjson.WriteError(w, r, err)
		return
	}
	view, err := s.deps.Dispatch.RequestPermit(r.Context(), actor, r.PathValue("code"), r.Header.Get("Idempotency-Key"))
	if err != nil {
		httpjson.WriteError(w, r, err)
		return
	}
	httpjson.WriteData(w, r, http.StatusOK, view)
}

// handleApproveParty is the ranger permit review.
func (s *Server) handleApproveParty(w http.ResponseWriter, r *http.Request) {
	actor, err := httpjson.RequireActor(r.Context())
	if err != nil {
		httpjson.WriteError(w, r, err)
		return
	}
	view, err := s.deps.Dispatch.ApproveParty(r.Context(), actor, r.PathValue("code"))
	if err != nil {
		httpjson.WriteError(w, r, err)
		return
	}
	httpjson.WriteData(w, r, http.StatusOK, view)
}

// handleDispatchParty releases a confirmed party onto the trail.
func (s *Server) handleDispatchParty(w http.ResponseWriter, r *http.Request) {
	actor, err := httpjson.RequireActor(r.Context())
	if err != nil {
		httpjson.WriteError(w, r, err)
		return
	}
	view, err := s.deps.Dispatch.DispatchParty(r.Context(), actor, r.PathValue("code"))
	if err != nil {
		httpjson.WriteError(w, r, err)
		return
	}
	httpjson.WriteData(w, r, http.StatusOK, view)
}

// reportCheckpointRequest is the body of the checkpoint report endpoint.
type reportCheckpointRequest struct {
	Seq       int    `json:"seq"`
	HeadCount int    `json:"head_count"`
	Note      string `json:"note"`
}

// handleReportCheckpoint records the progress of a party.
func (s *Server) handleReportCheckpoint(w http.ResponseWriter, r *http.Request) {
	actor, err := httpjson.RequireActor(r.Context())
	if err != nil {
		httpjson.WriteError(w, r, err)
		return
	}
	var body reportCheckpointRequest
	if err := httpjson.Decode(r, &body); err != nil {
		httpjson.WriteError(w, r, err)
		return
	}
	view, err := s.deps.Dispatch.ReportCheckpoint(r.Context(), actor, r.PathValue("code"), dispatch.ReportInput{
		Seq:       body.Seq,
		HeadCount: body.HeadCount,
		Note:      body.Note,
	})
	if err != nil {
		httpjson.WriteError(w, r, err)
		return
	}
	httpjson.WriteData(w, r, http.StatusCreated, view)
}

// handleCompleteParty closes a finished trip.
func (s *Server) handleCompleteParty(w http.ResponseWriter, r *http.Request) {
	actor, err := httpjson.RequireActor(r.Context())
	if err != nil {
		httpjson.WriteError(w, r, err)
		return
	}
	view, err := s.deps.Dispatch.CompleteParty(r.Context(), actor, r.PathValue("code"))
	if err != nil {
		httpjson.WriteError(w, r, err)
		return
	}
	httpjson.WriteData(w, r, http.StatusOK, view)
}

// cancelPartyRequest is the body of the cancellation endpoint.
type cancelPartyRequest struct {
	Reason string `json:"reason"`
}

// handleCancelParty withdraws a party before it enters the trail.
func (s *Server) handleCancelParty(w http.ResponseWriter, r *http.Request) {
	actor, err := httpjson.RequireActor(r.Context())
	if err != nil {
		httpjson.WriteError(w, r, err)
		return
	}
	var body cancelPartyRequest
	if err := httpjson.Decode(r, &body); err != nil {
		httpjson.WriteError(w, r, err)
		return
	}
	view, err := s.deps.Dispatch.CancelParty(r.Context(), actor, r.PathValue("code"), body.Reason)
	if err != nil {
		httpjson.WriteError(w, r, err)
		return
	}
	httpjson.WriteData(w, r, http.StatusOK, view)
}

// handleListParties returns a filtered page of parties.
func (s *Server) handleListParties(w http.ResponseWriter, r *http.Request) {
	actor, err := httpjson.RequireActor(r.Context())
	if err != nil {
		httpjson.WriteError(w, r, err)
		return
	}
	page, err := httpjson.PageRequest(r, sqliterepo.PartySortKeys())
	if err != nil {
		httpjson.WriteError(w, r, err)
		return
	}
	filter := domain.PartyFilter{
		HikeDayGE: r.URL.Query().Get("hike_day_from"),
		HikeDayLE: r.URL.Query().Get("hike_day_to"),
		Search:    r.URL.Query().Get("search"),
	}
	for _, state := range httpjson.QueryCSV(r, "state") {
		filter.States = append(filter.States, domain.PartyState(state))
	}
	if code := r.URL.Query().Get("trail_code"); code != "" {
		trailID, err := s.deps.Catalog.TrailIDByCode(r.Context(), code)
		if err != nil {
			httpjson.WriteError(w, r, err)
			return
		}
		filter.TrailID = trailID
	}
	result, err := s.deps.Dispatch.ListParties(r.Context(), actor, filter, page)
	if err != nil {
		httpjson.WriteError(w, r, err)
		return
	}
	httpjson.WriteData(w, r, http.StatusOK, result)
}

// handleGetParty returns one party with members and reports.
func (s *Server) handleGetParty(w http.ResponseWriter, r *http.Request) {
	actor, err := httpjson.RequireActor(r.Context())
	if err != nil {
		httpjson.WriteError(w, r, err)
		return
	}
	view, err := s.deps.Dispatch.GetParty(r.Context(), actor, r.PathValue("code"))
	if err != nil {
		httpjson.WriteError(w, r, err)
		return
	}
	httpjson.WriteData(w, r, http.StatusOK, view)
}

// parseTimestamp parses an RFC3339 timestamp field.
func parseTimestamp(raw, field string) (time.Time, error) {
	if raw == "" {
		return time.Time{}, apperr.Newf(apperr.CodeInvalidArgument, "%s 不能为空", field).WithField(field)
	}
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, apperr.Wrapf(apperr.CodeInvalidArgument, err, "%s 必须是 RFC3339 时间", field).WithField(field)
	}
	return parsed, nil
}
