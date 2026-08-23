//go:build e2e

package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// TestOptionalRealLLMSmoke is intentionally not a merge gate. It accepts a
// complete OpenAI-compatible chat-completions endpoint so credentials never
// need to enter repository configuration or test logs.
func TestOptionalRealLLMSmoke(t *testing.T) {
	endpoint := strings.TrimSpace(os.Getenv("AGENTICRAG_E2E_LLM_URL"))
	token := strings.TrimSpace(os.Getenv("AGENTICRAG_E2E_LLM_TOKEN"))
	model := strings.TrimSpace(os.Getenv("AGENTICRAG_E2E_LLM_MODEL"))
	if endpoint == "" || token == "" || model == "" {
		t.Skip("real LLM smoke requires AGENTICRAG_E2E_LLM_URL, AGENTICRAG_E2E_LLM_TOKEN, and AGENTICRAG_E2E_LLM_MODEL")
	}

	payload, err := json.Marshal(map[string]any{
		"model":      model,
		"messages":   []map[string]string{{"role": "user", "content": "Reply with exactly: agenticrag-smoke"}},
		"max_tokens": 16,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "agenticrag-e2e-real-smoke")
	response, err := (&http.Client{Timeout: 20 * time.Second}).Do(request)
	if err != nil {
		t.Fatalf("real LLM smoke transport failed: %v", err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		t.Fatalf("real LLM smoke status=%d response_bytes=%d", response.StatusCode, len(data))
	}
	var decoded struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("decode real LLM smoke response: %v", err)
	}
	if len(decoded.Choices) == 0 || strings.TrimSpace(decoded.Choices[0].Message.Content) == "" {
		t.Fatal("real LLM smoke returned no compatible assistant content")
	}
}
