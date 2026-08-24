package redaction

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
)

func FuzzRedactNeverLeaksConstructedBearer(f *testing.F) {
	f.Add([]byte("seed"))
	f.Add([]byte(`{"password":"secret"}`))
	service, _ := NewStatic(StrictPolicy())
	f.Fuzz(func(t *testing.T, input []byte) {
		digest := fmt.Sprintf("%x", sha256.Sum256(input))
		credential := "Bearer " + digest
		result := service.Redact(context.Background(), "ten_fuzz", BoundaryLog, string(input)+" "+credential)
		if strings.Contains(result.Value, credential) || len(result.Value) > (64<<10) {
			t.Fatalf("unsafe bounded result length=%d", len(result.Value))
		}
		jsonResult := service.RedactJSON(context.Background(), "ten_fuzz", BoundaryArtifactPreview, input)
		if len(jsonResult.Value) > (1 << 20) {
			t.Fatalf("unbounded JSON result length=%d", len(jsonResult.Value))
		}
	})
}
