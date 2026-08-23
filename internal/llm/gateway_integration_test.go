package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestGatewayHTTPFakeProviderIntegration(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Idempotency-Key") != "op-0001" {
			t.Errorf("idempotency key=%q", request.Header.Get("Idempotency-Key"))
		}
		if calls.Add(1) == 1 {
			http.Error(writer, "temporarily unavailable", http.StatusServiceUnavailable)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(struct {
			Text              string
			FinishReason      FinishReason
			Usage             Usage
			ProviderRequestID string
		}{Text: "network answer", FinishReason: FinishReasonStop,
			Usage: Usage{InputTokens: 4, OutputTokens: 2, TotalTokens: 6}, ProviderRequestID: "http-request-1"})
	}))
	defer server.Close()

	provider := ProviderFunc(func(ctx context.Context, request ProviderRequest) (ProviderResponse, error) {
		httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL, nil)
		if err != nil {
			return ProviderResponse{}, err
		}
		httpRequest.Header.Set("Idempotency-Key", request.OperationKey)
		response, err := server.Client().Do(httpRequest)
		if err != nil {
			value, _ := NewProviderError(ErrorUnavailable, 0, err)
			return ProviderResponse{}, value
		}
		defer response.Body.Close()
		if response.StatusCode == http.StatusServiceUnavailable {
			value, _ := NewProviderError(ErrorUnavailable, 0, errors.New("fake provider unavailable"))
			return ProviderResponse{}, value
		}
		var decoded ProviderResponse
		if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
			return ProviderResponse{}, err
		}
		return decoded, nil
	})
	gateway := testGateway(t, provider, Price{InputNanosPerMillionTokens: 1_000_000_000,
		OutputNanosPerMillionTokens: 2_000_000_000}, acceptingValidator(), &recordingRecorder{}, testGatewayConfig())
	result, err := gateway.Generate(context.Background(), validTextRequest())
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 || result.Attempts != 2 || result.Response.Text != "network answer" || result.CostNanos != 8_000 {
		t.Fatalf("calls=%d result=%+v", calls.Load(), result)
	}
}
