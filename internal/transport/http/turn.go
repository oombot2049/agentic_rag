package httptransport

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/agenticrag/agenticrag/internal/apperror"
	"github.com/agenticrag/agenticrag/internal/auth"
	"github.com/agenticrag/agenticrag/internal/queue"
	"github.com/agenticrag/agenticrag/internal/turn"
)

const maximumCreateTurnBody = 140 << 10

type TurnHandler struct{ service *turn.Service }

type optionalTurnExecution struct {
	present  bool
	maxSteps *int
}

func (o *optionalTurnExecution) UnmarshalJSON(value []byte) error {
	o.present = true
	if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		return errors.New("execution must be an object")
	}
	decoder := json.NewDecoder(bytes.NewReader(value))
	decoder.DisallowUnknownFields()
	var decoded struct {
		MaxSteps *int `json:"max_steps"`
	}
	if err := decoder.Decode(&decoded); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("execution contains trailing JSON")
	}
	o.maxSteps = decoded.MaxSteps
	return nil
}

func NewTurnHandler(service *turn.Service) (*TurnHandler, error) {
	if service == nil {
		return nil, errors.New("turn service is required")
	}
	return &TurnHandler{service: service}, nil
}

func (h *TurnHandler) Routes() http.Handler {
	mux := http.NewServeMux()
	write, _ := auth.RequirePermission("sessions:write", nil)
	mux.Handle("POST /v1/sessions/{session_id}/turns", write(http.HandlerFunc(h.create)))
	return mux
}

func (h *TurnHandler) create(response http.ResponseWriter, request *http.Request) {
	principal, ok := auth.PrincipalFromContext(request.Context())
	if !ok {
		auth.WriteFailure(response, request, apperror.New(apperror.CodeUnauthenticated, false, nil))
		return
	}
	key, err := singleHeader(request, "Idempotency-Key")
	if err != nil {
		auth.WriteFailure(response, request, apperror.New(apperror.CodeInvalidIdempotencyKey, false, err))
		return
	}
	if err := requireJSON(request.Header.Get("Content-Type")); err != nil {
		auth.WriteFailure(response, request, apperror.New(apperror.CodeInvalidArgument, false, err))
		return
	}
	request.Body = http.MaxBytesReader(response, request.Body, maximumCreateTurnBody)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	var body *struct {
		ExpectedContextVersion *uint64 `json:"expected_context_version"`
		Message                *struct {
			Content *string `json:"content"`
		} `json:"message"`
		Execution optionalTurnExecution `json:"execution"`
	}
	if err := decoder.Decode(&body); err != nil {
		auth.WriteFailure(response, request, apperror.New(apperror.CodeInvalidArgument, false, err))
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		auth.WriteFailure(response, request, apperror.New(apperror.CodeInvalidArgument, false, errors.New("request body contains trailing JSON")))
		return
	}
	if body == nil || body.ExpectedContextVersion == nil || body.Message == nil || body.Message.Content == nil {
		auth.WriteFailure(response, request, apperror.New(apperror.CodeInvalidArgument, false, errors.New("expected_context_version and message.content are required")))
		return
	}
	traceParent, err := optionalSingleHeader(request, "Traceparent")
	if err != nil {
		auth.WriteFailure(response, request, apperror.New(apperror.CodeInvalidArgument, false, err))
		return
	}
	traceState, err := optionalSingleHeader(request, "Tracestate")
	if err != nil {
		auth.WriteFailure(response, request, apperror.New(apperror.CodeInvalidArgument, false, err))
		return
	}
	trace, err := queue.NewTraceContext(traceParent, traceState)
	if err != nil {
		auth.WriteFailure(response, request, apperror.New(apperror.CodeInvalidArgument, false, err))
		return
	}
	var maxSteps *int
	if body.Execution.present {
		maxSteps = body.Execution.maxSteps
	}
	created, err := h.service.Create(request.Context(), turn.CreateRequest{
		TenantID: principal.TenantID(), AuthenticatedSubjectID: principal.SubjectID(),
		SessionID: request.PathValue("session_id"), IdempotencyKey: key,
		ExpectedContextVersion: *body.ExpectedContextVersion, MessageContent: *body.Message.Content, MaxSteps: maxSteps, Trace: trace,
	})
	if err != nil {
		auth.WriteFailure(response, request, err)
		return
	}
	setSuccessHeaders(response)
	response.Header().Set("Location", "/v1/runs/"+created.RunID)
	response.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(response).Encode(created)
}
