package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/dashboard"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/scope"
)

const dashboardBodyLimit = dashboard.MaxDefinitionBytes + 2048

type dashboardCapabilities struct {
	Enabled    bool `json:"enabled"`
	CanEdit    bool `json:"canEdit"`
	CanPublish bool `json:"canPublish"`
	MaxDrafts  int  `json:"maxDrafts"`
	MaxWidgets int  `json:"maxWidgets"`
}

func (s *Server) dashboardAvailable(ctx context.Context) bool {
	return s.deps.DashboardStore != nil && s.deps.DashboardStore.Ready(ctx) == nil
}
func (s *Server) handleDashboardCapabilities(w http.ResponseWriter, r *http.Request) {
	sc := scope.From(r.Context())
	ok := s.dashboardAvailable(r.Context())
	writeJSON(w, http.StatusOK, dashboardCapabilities{Enabled: ok, CanEdit: ok && sc.CanEditDashboard, CanPublish: ok && sc.CanPublishDashboard, MaxDrafts: dashboard.MaxDraftsPerOwner, MaxWidgets: 24})
}
func (s *Server) dashboardRefs() map[string]struct{} {
	m := make(map[string]struct{}, len(s.deps.DashboardQueryRefs))
	for _, r := range s.deps.DashboardQueryRefs {
		m[r] = struct{}{}
	}
	return m
}
func (s *Server) requireDashboard(w http.ResponseWriter, r *http.Request, edit, publish bool) (scope.Scope, bool) {
	sc := scope.From(r.Context())
	if s.deps.DashboardStore == nil {
		writeError(w, r, http.StatusServiceUnavailable, "dashboard_store_unavailable", "Dashboard builder is unavailable.")
		return sc, false
	}
	if (edit && !sc.CanEditDashboard) || (publish && !sc.CanPublishDashboard) || sc.Subject == "" {
		writeError(w, r, http.StatusForbidden, "forbidden", "Dashboard permission is required.")
		return sc, false
	}
	return sc, true
}

func (s *Server) handleDashboardList(w http.ResponseWriter, r *http.Request) {
	sc := scope.From(r.Context())
	if s.deps.DashboardStore == nil || (!sc.CanEditDashboard && !sc.CanPublishDashboard) || sc.Subject == "" {
		if s.deps.DashboardStore == nil {
			writeError(w, r, http.StatusServiceUnavailable, "dashboard_store_unavailable", "Dashboard builder is unavailable.")
		} else {
			writeError(w, r, http.StatusForbidden, "forbidden", "Dashboard permission is required.")
		}
		return
	}
	limit := dashboard.MaxListPage
	if raw := r.URL.Query().Get("limit"); raw != "" {
		var err error
		limit, err = strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > dashboard.MaxListPage {
			writeError(w, r, 400, "invalid_page", "limit must be between 1 and 50.")
			return
		}
	}
	page, err := s.deps.DashboardStore.List(r.Context(), sc.Subject, sc.CanPublishDashboard, r.URL.Query().Get("cursor"), limit)
	if err != nil {
		s.writeDashboardResult(w, r, nil, err)
		return
	}
	for i := range page.Items {
		if !s.validateStoredDashboard(w, r, page.Items[i]) {
			return
		}
		page.Items[i].Owned = page.Items[i].Owner == sc.Subject
	}
	s.writeDashboardResult(w, r, page, nil)
}
func (s *Server) handleDashboardCreate(w http.ResponseWriter, r *http.Request) {
	sc, ok := s.requireDashboard(w, r, true, false)
	if !ok {
		return
	}
	def, ok := s.readDefinition(w, r)
	if !ok {
		return
	}
	d, err := s.deps.DashboardStore.Create(r.Context(), sc.Subject, def)
	if err != nil {
		s.writeDashboardResult(w, r, nil, err)
		return
	}
	d.Owned = true
	w.Header().Set("ETag", revisionETag(d.Revision))
	writeJSON(w, http.StatusCreated, d)
}
func (s *Server) handleDashboardGet(w http.ResponseWriter, r *http.Request) {
	sc := scope.From(r.Context())
	if s.deps.DashboardStore == nil {
		writeError(w, r, http.StatusServiceUnavailable, "dashboard_store_unavailable", "Dashboard builder is unavailable.")
		return
	}
	if (!sc.CanEditDashboard && !sc.CanPublishDashboard) || sc.Subject == "" {
		writeError(w, r, http.StatusForbidden, "forbidden", "Dashboard permission is required.")
		return
	}
	if !validDraftID(w, r) {
		return
	}
	d, err := s.deps.DashboardStore.Get(r.Context(), r.PathValue("id"), sc.Subject, sc.CanPublishDashboard)
	if err == nil {
		if !s.validateStoredDashboard(w, r, d) {
			return
		}
		d.Owned = d.Owner == sc.Subject
		w.Header().Set("ETag", revisionETag(d.Revision))
	}
	s.writeDashboardResult(w, r, d, err)
}
func (s *Server) handleDashboardUpdate(w http.ResponseWriter, r *http.Request) {
	if !validDraftID(w, r) {
		return
	}
	sc, ok := s.requireDashboard(w, r, true, false)
	if !ok {
		return
	}
	rev, ok := requireRevision(w, r)
	if !ok {
		return
	}
	def, ok := s.readDefinition(w, r)
	if !ok {
		return
	}
	d, err := s.deps.DashboardStore.Update(r.Context(), r.PathValue("id"), sc.Subject, rev, def)
	if err == nil {
		d.Owned = true
		w.Header().Set("ETag", revisionETag(d.Revision))
	}
	s.writeDashboardResult(w, r, d, err)
}
func (s *Server) handleDashboardDelete(w http.ResponseWriter, r *http.Request) {
	if !validDraftID(w, r) {
		return
	}
	sc, ok := s.requireDashboard(w, r, true, false)
	if !ok {
		return
	}
	rev, ok := requireRevision(w, r)
	if !ok {
		return
	}
	err := s.deps.DashboardStore.Delete(r.Context(), r.PathValue("id"), sc.Subject, rev)
	if err != nil {
		s.writeDashboardResult(w, r, nil, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func (s *Server) handleDashboardSubmit(w http.ResponseWriter, r *http.Request) {
	if !validDraftID(w, r) {
		return
	}
	sc, ok := s.requireDashboard(w, r, true, false)
	if !ok {
		return
	}
	rev, ok := requireRevision(w, r)
	if !ok {
		return
	}
	current, err := s.deps.DashboardStore.Get(r.Context(), r.PathValue("id"), sc.Subject, false)
	if err != nil {
		s.writeDashboardResult(w, r, nil, err)
		return
	}
	if !s.validateStoredDashboard(w, r, current) {
		return
	}
	d, err := s.deps.DashboardStore.Submit(r.Context(), r.PathValue("id"), sc.Subject, rev)
	if err == nil {
		d.Owned = true
		w.Header().Set("ETag", revisionETag(d.Revision))
	}
	s.writeDashboardResult(w, r, d, err)
}
func (s *Server) handleDashboardApprove(w http.ResponseWriter, r *http.Request) {
	if !validDraftID(w, r) {
		return
	}
	sc, ok := s.requireDashboard(w, r, false, true)
	if !ok {
		return
	}
	rev, ok := requireRevision(w, r)
	if !ok {
		return
	}
	current, err := s.deps.DashboardStore.Get(r.Context(), r.PathValue("id"), sc.Subject, true)
	if err != nil {
		s.writeDashboardResult(w, r, nil, err)
		return
	}
	if !s.validateStoredDashboard(w, r, current) {
		return
	}
	d, err := s.deps.DashboardStore.Approve(r.Context(), r.PathValue("id"), rev)
	if err == nil {
		d.Owned = d.Owner == sc.Subject
		w.Header().Set("ETag", revisionETag(d.Revision))
	}
	s.writeDashboardResult(w, r, d, err)
}
func (s *Server) handleDashboardClone(w http.ResponseWriter, r *http.Request) {
	if !validDraftID(w, r) {
		return
	}
	sc, ok := s.requireDashboard(w, r, true, false)
	if !ok {
		return
	}
	source, err := s.deps.DashboardStore.Get(r.Context(), r.PathValue("id"), sc.Subject, false)
	if errors.Is(err, dashboard.ErrNotFound) {
		approved, e := s.deps.DashboardStore.Get(r.Context(), r.PathValue("id"), sc.Subject, true)
		if e == nil && approved.State == dashboard.StateApproved {
			source, err = approved, nil
		}
	}
	if err != nil || (!strings.EqualFold(source.Owner, sc.Subject) && source.State != dashboard.StateApproved) {
		s.writeDashboardResult(w, r, nil, dashboard.ErrNotFound)
		return
	}
	if err := dashboard.Validate(source.Definition, s.dashboardRefs()); err != nil {
		writeError(w, r, http.StatusInternalServerError, "invalid_snapshot", "Dashboard snapshot is invalid.")
		return
	}
	d, err := s.deps.DashboardStore.Create(r.Context(), sc.Subject, source.Definition)
	if err == nil {
		d.Owned = true
		w.Header().Set("ETag", revisionETag(d.Revision))
	}
	s.writeDashboardResult(w, r, d, err)
}
func (s *Server) handleDashboardExport(w http.ResponseWriter, r *http.Request) {
	if !validDraftID(w, r) {
		return
	}
	sc := scope.From(r.Context())
	if s.deps.DashboardStore == nil {
		writeError(w, r, http.StatusServiceUnavailable, "dashboard_store_unavailable", "Dashboard builder is unavailable.")
		return
	}
	if sc.Subject == "" || (!sc.CanEditDashboard && !sc.CanPublishDashboard) {
		writeError(w, r, http.StatusForbidden, "forbidden", "Dashboard permission is required.")
		return
	}
	d, err := s.deps.DashboardStore.Get(r.Context(), r.PathValue("id"), sc.Subject, sc.CanPublishDashboard)
	if err != nil || d.State != dashboard.StateApproved {
		s.writeDashboardResult(w, r, nil, dashboard.ErrNotFound)
		return
	}
	if err := dashboard.Validate(d.Definition, s.dashboardRefs()); err != nil {
		writeError(w, r, http.StatusInternalServerError, "invalid_snapshot", "Approved snapshot is invalid.")
		return
	}
	b, sha, err := dashboard.Canonical(d.Definition)
	if err != nil {
		writeError(w, r, 500, "internal", "Export failed.")
		return
	}
	etag := `"sha256-` + sha + `"`
	w.Header().Set("ETag", etag)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.json"`, d.Definition.ID))
	w.Write(b)
}

// handleDashboardImport는 export한 canonical JSON(정의 본문 그 자체)을 업로드받아
// 요청자 소유 draft로 생성합니다. Export와 대칭이며, 서버가 queryRef allowlist·closed
// widget 규칙으로 재검증하므로 raw query나 임의 component는 유입되지 않습니다. (ADR 0016)
func (s *Server) handleDashboardImport(w http.ResponseWriter, r *http.Request) {
	sc, ok := s.requireDashboard(w, r, true, false)
	if !ok {
		return
	}
	def, ok := s.readCanonicalDefinition(w, r)
	if !ok {
		return
	}
	d, err := s.deps.DashboardStore.Create(r.Context(), sc.Subject, def)
	if err != nil {
		s.writeDashboardResult(w, r, nil, err)
		return
	}
	d.Owned = true
	w.Header().Set("ETag", revisionETag(d.Revision))
	writeJSON(w, http.StatusCreated, d)
}

// readCanonicalDefinition은 wrapper 없는 순수 Definition JSON(export 산출물)을 읽어 검증합니다.
func (s *Server) readCanonicalDefinition(w http.ResponseWriter, r *http.Request) (dashboard.Definition, bool) {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		writeError(w, r, 415, "unsupported_media_type", "Content-Type application/json is required.")
		return dashboard.Definition{}, false
	}
	r.Body = http.MaxBytesReader(w, r.Body, dashboardBodyLimit)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, r, http.StatusRequestEntityTooLarge, "body_too_large", "Dashboard request is too large.")
		return dashboard.Definition{}, false
	}
	if err := dashboard.ValidateJSONTokens(raw); err != nil {
		writeError(w, r, 400, "invalid_dashboard", err.Error())
		return dashboard.Definition{}, false
	}
	def, err := dashboard.DecodeAndValidate(raw, s.dashboardRefs())
	if err != nil {
		writeError(w, r, 400, "invalid_dashboard", err.Error())
		return dashboard.Definition{}, false
	}
	return def, true
}

func (s *Server) readDefinition(w http.ResponseWriter, r *http.Request) (dashboard.Definition, bool) {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		writeError(w, r, 415, "unsupported_media_type", "Content-Type application/json is required.")
		return dashboard.Definition{}, false
	}
	r.Body = http.MaxBytesReader(w, r.Body, dashboardBodyLimit)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, r, http.StatusRequestEntityTooLarge, "body_too_large", "Dashboard request is too large.")
		return dashboard.Definition{}, false
	}
	if err := dashboard.ValidateJSONTokens(raw); err != nil {
		writeError(w, r, 400, "invalid_dashboard", err.Error())
		return dashboard.Definition{}, false
	}
	var body struct {
		Definition json.RawMessage `json:"definition"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil || len(body.Definition) == 0 {
		writeError(w, r, 400, "invalid_dashboard", "A definition is required.")
		return dashboard.Definition{}, false
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		writeError(w, r, 400, "invalid_dashboard", "Trailing JSON is forbidden.")
		return dashboard.Definition{}, false
	}
	def, err := dashboard.DecodeAndValidate(body.Definition, s.dashboardRefs())
	if err != nil {
		writeError(w, r, 400, "invalid_dashboard", err.Error())
		return dashboard.Definition{}, false
	}
	return def, true
}
func validDraftID(w http.ResponseWriter, r *http.Request) bool {
	if _, err := uuid.Parse(r.PathValue("id")); err != nil {
		writeError(w, r, 404, "not_found", "Dashboard was not found.")
		return false
	}
	return true
}
func revisionETag(rev int64) string { return fmt.Sprintf(`"revision-%d"`, rev) }
func requireRevision(w http.ResponseWriter, r *http.Request) (int64, bool) {
	h := r.Header.Get("If-Match")
	if h == "" {
		writeError(w, r, 428, "precondition_required", "If-Match revision is required.")
		return 0, false
	}
	var rev int64
	if _, err := fmt.Sscanf(h, `"revision-%d"`, &rev); err != nil || h != revisionETag(rev) || rev < 1 {
		writeError(w, r, 400, "invalid_revision", "If-Match revision is invalid.")
		return 0, false
	}
	return rev, true
}
func (s *Server) writeDashboardResult(w http.ResponseWriter, r *http.Request, value any, err error) {
	switch {
	case err == nil:
		writeJSON(w, 200, value)
	case errors.Is(err, dashboard.ErrNotFound):
		writeError(w, r, 404, "not_found", "Dashboard was not found.")
	case errors.Is(err, dashboard.ErrConflict):
		writeError(w, r, 409, "revision_conflict", "Dashboard changed; local edits were not overwritten.")
	case errors.Is(err, dashboard.ErrLimit):
		writeError(w, r, 409, "draft_limit", "Draft limit reached.")
	case errors.Is(err, dashboard.ErrImmutable):
		writeError(w, r, 409, "approved_immutable", "Approved dashboards are immutable.")
	case errors.Is(err, dashboard.ErrInvalidState):
		writeError(w, r, 409, "invalid_state", "Dashboard state transition is invalid.")
	case errors.Is(err, dashboard.ErrInvalidCursor):
		writeError(w, r, 400, "invalid_cursor", "Dashboard cursor is invalid.")
	default:
		writeError(w, r, 503, "dashboard_store_unavailable", "Dashboard metadata store is unavailable.")
	}
}

func (s *Server) validateStoredDashboard(w http.ResponseWriter, r *http.Request, d dashboard.Draft) bool {
	if err := dashboard.Validate(d.Definition, s.dashboardRefs()); err != nil {
		writeError(w, r, http.StatusInternalServerError, "invalid_snapshot", "Stored dashboard snapshot is invalid.")
		return false
	}
	return true
}
