package auth

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/agenticrag/agenticrag/internal/apperror"
)

const (
	maximumBearerLength = 8192
	maximumAPIKeyLength = 4096
)

type FailureHandler func(http.ResponseWriter, *http.Request, error)

func Middleware(authenticator *Authenticator, failureHandler FailureHandler) (func(http.Handler) http.Handler, error) {
	if authenticator == nil {
		return nil, errors.New("authenticator is required")
	}
	if failureHandler == nil {
		failureHandler = WriteFailure
	}
	return func(next http.Handler) http.Handler {
		if next == nil {
			return nil
		}
		return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
			credential, err := ExtractCredential(request)
			if err != nil {
				failureHandler(response, request, apperror.New(apperror.CodeUnauthenticated, false, err))
				return
			}
			principal, err := authenticator.Authenticate(request.Context(), credential)
			if err != nil {
				failureHandler(response, request, authenticationError(err))
				return
			}
			trustedRequest := request.Clone(withPrincipal(request.Context(), principal))
			trustedRequest.Header = request.Header.Clone()
			for _, untrusted := range []string{"Authorization", "X-API-Key", "X-Tenant-ID", "X-User-ID", "X-Subject-ID"} {
				trustedRequest.Header.Del(untrusted)
			}
			next.ServeHTTP(response, trustedRequest)
		})
	}, nil
}

func RequirePermission(permission string, failureHandler FailureHandler) (func(http.Handler) http.Handler, error) {
	if !permissionPattern.MatchString(permission) {
		return nil, ErrInvalidPrincipal
	}
	if failureHandler == nil {
		failureHandler = WriteFailure
	}
	return func(next http.Handler) http.Handler {
		if next == nil {
			return nil
		}
		return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
			principal, ok := PrincipalFromContext(request.Context())
			if !ok {
				failureHandler(response, request, apperror.New(apperror.CodeUnauthenticated, false, nil))
				return
			}
			if !principal.HasPermission(permission) {
				failureHandler(response, request, apperror.New(apperror.CodePermissionDenied, false, nil))
				return
			}
			next.ServeHTTP(response, request)
		})
	}, nil
}

func ExtractCredential(request *http.Request) (Credential, error) {
	if request == nil {
		return Credential{}, ErrMissingCredential
	}
	authorizations := request.Header.Values("Authorization")
	apiKeys := request.Header.Values("X-API-Key")
	if len(authorizations) > 1 || len(apiKeys) > 1 || (len(authorizations) == 1 && len(apiKeys) == 1) {
		return Credential{}, ErrMalformedCredential
	}
	if len(authorizations) == 1 {
		value := authorizations[0]
		if strings.Contains(value, ",") {
			return Credential{}, ErrMalformedCredential
		}
		parts := strings.Fields(value)
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || !validSecret(parts[1], 1, maximumBearerLength) {
			return Credential{}, ErrMalformedCredential
		}
		return Credential{Kind: CredentialBearer, Value: parts[1]}, nil
	}
	if len(apiKeys) == 1 {
		if strings.Contains(apiKeys[0], ",") || !validSecret(apiKeys[0], 16, maximumAPIKeyLength) {
			return Credential{}, ErrMalformedCredential
		}
		return Credential{Kind: CredentialAPIKey, Value: apiKeys[0]}, nil
	}
	return Credential{}, ErrMissingCredential
}

func WriteFailure(response http.ResponseWriter, _ *http.Request, err error) {
	public := apperror.Public(err)
	response.Header().Set("Content-Type", "application/json")
	response.Header().Set("Cache-Control", "no-store")
	response.Header().Set("X-Content-Type-Options", "nosniff")
	if public.Code == apperror.CodeUnauthenticated {
		response.Header().Set("WWW-Authenticate", `Bearer realm="agenticrag"`)
	}
	response.WriteHeader(apperror.HTTPStatus(err))
	_ = json.NewEncoder(response).Encode(struct {
		Error apperror.PublicError `json:"error"`
	}{Error: public})
}

func authenticationError(err error) error {
	switch {
	case errors.Is(err, ErrInvalidCredential), errors.Is(err, ErrMissingCredential), errors.Is(err, ErrMalformedCredential):
		return apperror.New(apperror.CodeUnauthenticated, false, err)
	case errors.Is(err, ErrVerifierUnavailable):
		return apperror.New(apperror.CodeServiceUnavailable, true, err)
	default:
		return apperror.New(apperror.CodeInternal, false, err)
	}
}
