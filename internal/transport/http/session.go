package httptransport

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"

	"github.com/agenticrag/agenticrag/internal/apperror"
	"github.com/agenticrag/agenticrag/internal/auth"
	"github.com/agenticrag/agenticrag/internal/session"
)

const maximumCreateSessionBody = 16 << 10

// SessionHandler exposes the authenticated Session command/query boundary.
type SessionHandler struct{ service *session.Service }

func NewSessionHandler(service *session.Service) (*SessionHandler, error) {
	if service == nil {
		return nil, errors.New("session service is required")
	}
	return &SessionHandler{service: service}, nil
}

// Routes builds an isolated route tree. Authentication middleware must wrap it;
// coarse route permissions are enforced here before application work.
func (h *SessionHandler) Routes() http.Handler {
	mux := http.NewServeMux()
	write, _ := auth.RequirePermission("sessions:write", nil)
	read, _ := auth.RequirePermission("sessions:read", nil)
	mux.Handle("POST /v1/sessions", write(http.HandlerFunc(h.create)))
	mux.Handle("GET /v1/sessions/{session_id}", read(http.HandlerFunc(h.get)))
	return mux
}

func (h *SessionHandler) create(response http.ResponseWriter, request *http.Request) {
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
	request.Body = http.MaxBytesReader(response, request.Body, maximumCreateSessionBody)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	var body *struct {
		Metadata session.Metadata `json:"metadata"`
	}
	if err := decoder.Decode(&body); err != nil {
		auth.WriteFailure(response, request, apperror.New(apperror.CodeInvalidArgument, false, err))
		return
	}
	if body == nil {
		auth.WriteFailure(response, request, apperror.New(apperror.CodeInvalidArgument, false, errors.New("request body must be an object")))
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		auth.WriteFailure(response, request, apperror.New(apperror.CodeInvalidArgument, false, errors.New("request body contains trailing JSON")))
		return
	}
	view, err := h.service.Create(request.Context(), session.CreateRequest{
		TenantID: principal.TenantID(), AuthenticatedSubjectID: principal.SubjectID(),
		IdempotencyKey: key, Metadata: body.Metadata,
	})
	if err != nil {
		auth.WriteFailure(response, request, err)
		return
	}
	location := "/v1/sessions/" + view.ID
	setSuccessHeaders(response)
	response.Header().Set("Location", location)
	response.Header().Set("ETag", session.ETag(view.RowVersion))
	response.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(response).Encode(view)
}

func (h *SessionHandler) get(response http.ResponseWriter, request *http.Request) {
	principal, ok := auth.PrincipalFromContext(request.Context())
	if !ok {
		auth.WriteFailure(response, request, apperror.New(apperror.CodeUnauthenticated, false, nil))
		return
	}
	view, err := h.service.Get(request.Context(), principal.TenantID(), request.PathValue("session_id"))
	if err != nil {
		auth.WriteFailure(response, request, err)
		return
	}
	setSuccessHeaders(response)
	response.Header().Set("ETag", session.ETag(view.RowVersion))
	_ = json.NewEncoder(response).Encode(view)
}

func singleHeader(request *http.Request, name string) (string, error) {
	values := request.Header.Values(name)
	if len(values) != 1 || strings.Contains(values[0], ",") {
		return "", errors.New("exactly one header value is required")
	}
	return values[0], nil
}

func optionalSingleHeader(request *http.Request, name string) (string, error) {
	values := request.Header.Values(name)
	if len(values) == 0 {
		return "", nil
	}
	if len(values) != 1 || values[0] == "" || strings.Contains(values[0], ",") {
		return "", errors.New("optional header must occur at most once and be non-empty")
	}
	return values[0], nil
}

func requireJSON(value string) error {
	mediaType, _, err := mime.ParseMediaType(value)
	if err != nil || !strings.EqualFold(mediaType, "application/json") {
		return errors.New("Content-Type must be application/json")
	}
	return nil
}

func setSuccessHeaders(response http.ResponseWriter) {
	response.Header().Set("Content-Type", "application/json")
	response.Header().Set("Cache-Control", "no-store")
	response.Header().Set("X-Content-Type-Options", "nosniff")
}
