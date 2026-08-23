package action

import (
	"encoding/json"
	"errors"
	"testing"
)

func FuzzDecodeNeverPanicsOrReturnsAmbiguousAction(f *testing.F) {
	seeds := []string{
		``, `{`, `null`, `{"thought":"secret"}`,
		`{"schema_version":1,"type":"SEARCH","reason_code":"SEARCH","search":{"query":"q","top_k":1,"knowledge_base_ids":["kb_0001"]}}`,
		`{"schema_version":1,"type":"TOOL","reason_code":"CALL","tool":{"name":"crm.lookup","arguments":{}}}`,
		`{"schema_version":1,"type":"FINAL","reason_code":"READY","final":{"answer":"ok","citation_ids":[]}}`,
	}
	for _, seed := range seeds {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		action, err := Decode(json.RawMessage(raw))
		if err != nil {
			if !errors.Is(err, ErrInvalidAction) {
				t.Fatalf("unexpected error identity: %v", err)
			}
			return
		}
		variants := 0
		if _, ok := action.Search(); ok {
			variants++
		}
		if _, ok := action.Tool(); ok {
			variants++
		}
		if _, ok := action.Final(); ok {
			variants++
		}
		if variants != 1 || action.ReasonCode() == "" {
			t.Fatalf("ambiguous Action: type=%s variants=%d", action.Type(), variants)
		}
		encoded, err := action.Encode()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Decode(encoded); err != nil {
			t.Fatalf("canonical Action did not round trip: %v", err)
		}
	})
}
