package redaction_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/agenticrag/agenticrag/internal/apperror"
	"github.com/agenticrag/agenticrag/internal/redaction"
)

func TestPublicErrorOmitsCauseAndInternalTelemetryRedactsIt(t *testing.T) {
	const secret = "password=never-return-this"
	internal := apperror.New(apperror.CodeInternal, false, errors.New(secret))
	encoded, err := json.Marshal(apperror.Public(internal))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), secret) || strings.Contains(string(encoded), "never-return-this") {
		t.Fatalf("public error leaked cause: %s", encoded)
	}
	service, err := redaction.NewStatic(redaction.StrictPolicy())
	if err != nil {
		t.Fatal(err)
	}
	result := service.Redact(context.Background(), "ten_0001", redaction.BoundaryErrorDetail, secret)
	if strings.Contains(result.Value, "never-return-this") || result.Value != redaction.Replacement {
		t.Fatalf("internal telemetry was not redacted: %+v", result)
	}
}
