package httptransport

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"

	"github.com/agenticrag/agenticrag/internal/apperror"
	"github.com/agenticrag/agenticrag/internal/auth"
	"github.com/agenticrag/agenticrag/internal/turn"
)

type QueryHandler struct{ service *turn.QueryService }

func NewQueryHandler(service *turn.QueryService) (*QueryHandler, error) {
	if service == nil {
		return nil, errors.New("query service is required")
	}
	return &QueryHandler{service: service}, nil
}

func (h *QueryHandler) Routes() http.Handler {
	mux := http.NewServeMux()
	read, _ := auth.RequirePermission("sessions:read", nil)
	mux.Handle("GET /v1/turns/{turn_id}", read(http.HandlerFunc(h.getTurn)))
	mux.Handle("GET /v1/runs/{run_id}", read(http.HandlerFunc(h.getRun)))
	mux.Handle("GET /v1/sessions/{session_id}/messages", read(http.HandlerFunc(h.listMessages)))
	return mux
}

func (h *QueryHandler) getTurn(response http.ResponseWriter, request *http.Request) {
	principal, ok := queryPrincipal(response, request)
	if !ok || !rejectQuery(response, request, nil) {
		return
	}
	view, err := h.service.GetTurn(request.Context(), principal.TenantID(), request.PathValue("turn_id"))
	writeQueryResult(response, request, view, err)
}

func (h *QueryHandler) getRun(response http.ResponseWriter, request *http.Request) {
	principal, ok := queryPrincipal(response, request)
	if !ok || !rejectQuery(response, request, nil) {
		return
	}
	view, err := h.service.GetRun(request.Context(), principal.TenantID(), request.PathValue("run_id"))
	writeQueryResult(response, request, view, err)
}

func (h *QueryHandler) listMessages(response http.ResponseWriter, request *http.Request) {
	principal, ok := queryPrincipal(response, request)
	if !ok {
		return
	}
	values, ok := parseAllowedQuery(response, request, map[string]struct{}{"cursor": {}, "page_size": {}})
	if !ok {
		return
	}
	pageSize := 0
	if raw, exists := values["page_size"]; exists {
		var err error
		pageSize, err = strconv.Atoi(raw[0])
		if err != nil || pageSize < 1 || pageSize > 100 {
			auth.WriteFailure(response, request, apperror.New(apperror.CodeInvalidArgument, false, err))
			return
		}
	}
	cursor := ""
	if raw, exists := values["cursor"]; exists {
		cursor = raw[0]
	}
	view, err := h.service.ListMessages(request.Context(), principal.TenantID(), request.PathValue("session_id"), cursor, pageSize)
	writeQueryResult(response, request, view, err)
}

func queryPrincipal(response http.ResponseWriter, request *http.Request) (auth.Principal, bool) {
	principal, ok := auth.PrincipalFromContext(request.Context())
	if !ok {
		auth.WriteFailure(response, request, apperror.New(apperror.CodeUnauthenticated, false, nil))
	}
	return principal, ok
}

func rejectQuery(response http.ResponseWriter, request *http.Request, allowed map[string]struct{}) bool {
	_, ok := parseAllowedQuery(response, request, allowed)
	return ok
}

func parseAllowedQuery(response http.ResponseWriter, request *http.Request, allowed map[string]struct{}) (url.Values, bool) {
	values, err := url.ParseQuery(request.URL.RawQuery)
	if err != nil {
		auth.WriteFailure(response, request, apperror.New(apperror.CodeInvalidArgument, false, err))
		return nil, false
	}
	for key, entries := range values {
		if _, exists := allowed[key]; !exists || len(entries) != 1 || entries[0] == "" {
			auth.WriteFailure(response, request, apperror.New(apperror.CodeInvalidArgument, false, errors.New("query parameter is unknown, empty, or duplicated")))
			return nil, false
		}
	}
	return values, true
}

func writeQueryResult(response http.ResponseWriter, request *http.Request, value any, err error) {
	if err != nil {
		auth.WriteFailure(response, request, err)
		return
	}
	setSuccessHeaders(response)
	_ = json.NewEncoder(response).Encode(value)
}
