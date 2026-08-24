package httptransport

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/agenticrag/agenticrag/internal/apperror"
	"github.com/agenticrag/agenticrag/internal/auth"
	runcontrol "github.com/agenticrag/agenticrag/internal/run"
)

type RunCancelHandler struct{ service *runcontrol.Service }

func NewRunCancelHandler(service *runcontrol.Service) (*RunCancelHandler, error) {
	if service == nil {
		return nil, errors.New("Run cancellation service is required")
	}
	return &RunCancelHandler{service: service}, nil
}

func (h *RunCancelHandler) Routes() http.Handler {
	mux := http.NewServeMux()
	write, _ := auth.RequirePermission("sessions:write", nil)
	mux.Handle("POST /v1/runs/{run_action}", write(http.HandlerFunc(h.cancel)))
	return mux
}

func (h *RunCancelHandler) cancel(response http.ResponseWriter, request *http.Request) {
	principal, ok := auth.PrincipalFromContext(request.Context())
	if !ok {
		auth.WriteFailure(response, request, apperror.New(apperror.CodeUnauthenticated, false, nil))
		return
	}
	runID, matches := strings.CutSuffix(request.PathValue("run_action"), ":cancel")
	if !matches || runID == "" {
		http.NotFound(response, request)
		return
	}
	if !rejectQuery(response, request, nil) {
		return
	}
	key, err := singleHeader(request, "Idempotency-Key")
	if err != nil {
		auth.WriteFailure(response, request, apperror.New(apperror.CodeInvalidIdempotencyKey, false, err))
		return
	}
	request.Body = http.MaxBytesReader(response, request.Body, 1)
	content, err := io.ReadAll(request.Body)
	if err != nil || len(content) != 0 {
		auth.WriteFailure(response, request, apperror.New(apperror.CodeInvalidArgument, false, errors.New("cancellation request body must be empty")))
		return
	}
	result, err := h.service.Cancel(request.Context(), runcontrol.CancelRequest{TenantID: principal.TenantID(),
		AuthenticatedSubjectID: principal.SubjectID(), RunID: runID, IdempotencyKey: key})
	if err != nil {
		auth.WriteFailure(response, request, err)
		return
	}
	setSuccessHeaders(response)
	response.Header().Set("Location", "/v1/runs/"+result.RunID)
	response.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(response).Encode(result)
}
