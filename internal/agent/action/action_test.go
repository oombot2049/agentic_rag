package action

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/agenticrag/agenticrag/internal/llm"
)

func TestDecodeAndCanonicalEncodeAllActionVariants(t *testing.T) {
	tests := []struct {
		name     string
		raw      string
		wantType Type
		assert   func(*testing.T, Action)
	}{
		{
			name: "search", wantType: TypeSearch,
			raw: `{"schema_version":1,"type":"SEARCH","reason_code":"NEED_EVIDENCE","search":{"query":"tenant policy","top_k":8,"knowledge_base_ids":["kb_0001","kb_0002"],"filters":{"language":"zh"}}}`,
			assert: func(t *testing.T, action Action) {
				search, ok := action.Search()
				if !ok || search.Query != "tenant policy" || search.TopK != 8 || len(search.KnowledgeBaseIDs) != 2 || string(search.Filters) != `{"language":"zh"}` {
					t.Fatalf("search=%+v", search)
				}
				search.KnowledgeBaseIDs[0] = "mutated"
				search.Filters[0] = 'x'
				again, _ := action.Search()
				if again.KnowledgeBaseIDs[0] != "kb_0001" || string(again.Filters) != `{"language":"zh"}` {
					t.Fatal("Search accessor exposed mutable state")
				}
			},
		},
		{
			name: "tool", wantType: TypeTool,
			raw: `{"schema_version":1,"type":"TOOL","reason_code":"VERIFY_ACCOUNT","tool":{"name":"crm.customer_lookup","arguments":{"customer_id":"123"}}}`,
			assert: func(t *testing.T, action Action) {
				tool, ok := action.Tool()
				if !ok || tool.Name != "crm.customer_lookup" || string(tool.Arguments) != `{"customer_id":"123"}` {
					t.Fatalf("tool=%+v", tool)
				}
			},
		},
		{
			name: "final", wantType: TypeFinal,
			raw: `{"schema_version":1,"type":"FINAL","reason_code":"ANSWER_READY","final":{"answer":"The answer.","citation_ids":["art_0001","art_0002"]}}`,
			assert: func(t *testing.T, action Action) {
				final, ok := action.Final()
				if !ok || final.Answer != "The answer." || len(final.CitationIDs) != 2 || final.CitationIDs[1] != "art_0002" {
					t.Fatalf("final=%+v", final)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			action, err := Decode(json.RawMessage(test.raw))
			if err != nil {
				t.Fatal(err)
			}
			if action.Type() != test.wantType || action.ReasonCode() == "" {
				t.Fatalf("action type/reason=%s/%s", action.Type(), action.ReasonCode())
			}
			test.assert(t, action)
			encoded, err := action.Encode()
			if err != nil {
				t.Fatal(err)
			}
			roundTrip, err := Decode(encoded)
			if err != nil || roundTrip.Type() != action.Type() || roundTrip.ReasonCode() != action.ReasonCode() {
				t.Fatalf("round trip=%+v error=%v encoded=%s", roundTrip, err, encoded)
			}
		})
	}
}

func TestDecodeRejectsAmbiguityBoundariesAndReasoningFields(t *testing.T) {
	secret := "SECRET_PRIVATE_REASONING_42"
	tests := []struct {
		name string
		raw  string
	}{
		{"unknown thought", `{"schema_version":1,"type":"FINAL","reason_code":"READY","thought":"` + secret + `","final":{"answer":"ok","citation_ids":[]}}`},
		{"unknown chain of thought", `{"schema_version":1,"type":"FINAL","reason_code":"READY","chain_of_thought":"` + secret + `","final":{"answer":"ok","citation_ids":[]}}`},
		{"free form reason", `{"schema_version":1,"type":"FINAL","reason_code":"READY","reason":"because","final":{"answer":"ok","citation_ids":[]}}`},
		{"case variant field", `{"schema_version":1,"Type":"FINAL","reason_code":"READY","final":{"answer":"ok","citation_ids":[]}}`},
		{"duplicate discriminator", `{"schema_version":1,"type":"FINAL","type":"TOOL","reason_code":"READY","final":{"answer":"ok","citation_ids":[]}}`},
		{"duplicate nested argument", `{"schema_version":1,"type":"TOOL","reason_code":"CALL","tool":{"name":"crm.lookup","arguments":{"id":1,"id":2}}}`},
		{"two payloads", `{"schema_version":1,"type":"FINAL","reason_code":"READY","final":{"answer":"ok","citation_ids":[]},"tool":{"name":"crm.lookup","arguments":{}}}`},
		{"wrong discriminator", `{"schema_version":1,"type":"SEARCH","reason_code":"READY","final":{"answer":"ok","citation_ids":[]}}`},
		{"bad reason code", `{"schema_version":1,"type":"FINAL","reason_code":"free form","final":{"answer":"ok","citation_ids":[]}}`},
		{"duplicate knowledge base", `{"schema_version":1,"type":"SEARCH","reason_code":"SEARCH","search":{"query":"q","top_k":1,"knowledge_base_ids":["kb_0001","kb_0001"]}}`},
		{"top k zero", `{"schema_version":1,"type":"SEARCH","reason_code":"SEARCH","search":{"query":"q","top_k":0,"knowledge_base_ids":["kb_0001"]}}`},
		{"filter is array", `{"schema_version":1,"type":"SEARCH","reason_code":"SEARCH","search":{"query":"q","top_k":1,"knowledge_base_ids":["kb_0001"],"filters":[]}}`},
		{"tool is not dotted", `{"schema_version":1,"type":"TOOL","reason_code":"CALL","tool":{"name":"lookup","arguments":{}}}`},
		{"arguments are array", `{"schema_version":1,"type":"TOOL","reason_code":"CALL","tool":{"name":"crm.lookup","arguments":[]}}`},
		{"duplicate citation", `{"schema_version":1,"type":"FINAL","reason_code":"READY","final":{"answer":"ok","citation_ids":["art_0001","art_0001"]}}`},
		{"bad citation", `{"schema_version":1,"type":"FINAL","reason_code":"READY","final":{"answer":"ok","citation_ids":["file_0001"]}}`},
		{"blank answer", `{"schema_version":1,"type":"FINAL","reason_code":"READY","final":{"answer":" ","citation_ids":[]}}`},
		{"missing required citations", `{"schema_version":1,"type":"FINAL","reason_code":"READY","final":{"answer":"ok"}}`},
		{"trailing value", `{"schema_version":1,"type":"FINAL","reason_code":"READY","final":{"answer":"ok","citation_ids":[]}} {}`},
		{"null", `null`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Decode(json.RawMessage(test.raw))
			if !errors.Is(err, ErrInvalidAction) {
				t.Fatalf("error=%v", err)
			}
			if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "because") {
				t.Fatalf("error disclosed raw model content: %v", err)
			}
		})
	}
}

func TestParserRepairsExactlyOnceAndNeverReturnsRawOutput(t *testing.T) {
	invalid := json.RawMessage(`{"schema_version":1,"type":"FINAL","reason_code":"READY","thought":"private","final":{"answer":"ok","citation_ids":[]}}`)
	valid := json.RawMessage(`{"schema_version":1,"type":"FINAL","reason_code":"READY","final":{"answer":"fixed","citation_ids":[]}}`)
	calls := 0
	parser, err := NewParser(RepairerFunc(func(_ context.Context, request RepairRequest) (json.RawMessage, error) {
		calls++
		if request.FailureCode != FailureContract || string(request.InvalidJSON) != string(invalid) {
			t.Fatalf("repair request=%+v", request)
		}
		request.InvalidJSON[0] = 'x'
		return append(json.RawMessage(nil), valid...), nil
	}), DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	result, err := parser.Parse(context.Background(), invalid)
	if err != nil || !result.Repaired || calls != 1 || result.Action.ReasonCode() != "READY" {
		t.Fatalf("result=%+v error=%v calls=%d", result, err, calls)
	}
	final, _ := result.Action.Final()
	if final.Answer != "fixed" || string(invalid)[0] != '{' {
		t.Fatalf("final=%+v input=%s", final, invalid)
	}

	badParser, _ := NewParser(RepairerFunc(func(context.Context, RepairRequest) (json.RawMessage, error) {
		calls++
		return json.RawMessage(`{"still":"invalid"}`), nil
	}), DefaultConfig())
	_, err = badParser.Parse(context.Background(), invalid)
	var parseErr *ParseError
	if !errors.As(err, &parseErr) || parseErr.Code() != FailureRepair || calls != 2 {
		t.Fatalf("error=%v calls=%d", err, calls)
	}
}

func TestParserDoesNotRepairOversizeOrCancelledInput(t *testing.T) {
	calls := 0
	repairer := RepairerFunc(func(context.Context, RepairRequest) (json.RawMessage, error) {
		calls++
		return nil, errors.New("must not run")
	})
	parser, _ := NewParser(repairer, Config{MaximumRawBytes: 64})
	if _, err := parser.Parse(context.Background(), json.RawMessage(strings.Repeat("x", 65))); !errors.Is(err, ErrInvalidAction) {
		t.Fatalf("oversize error=%v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := parser.Parse(ctx, json.RawMessage(`{`)); !errors.Is(err, ErrInvalidAction) {
		t.Fatalf("cancelled error=%v", err)
	}
	if calls != 0 {
		t.Fatalf("repair calls=%d", calls)
	}
}

func TestValidatorSchemaCopiesAndLLMGatewayContract(t *testing.T) {
	var _ llm.StructuredValidator = (*Validator)(nil)
	validator := NewValidator()
	schema := validator.Schema()
	if !json.Valid(schema) || !strings.Contains(string(schema), `"additionalProperties":false`) ||
		strings.Contains(string(schema), `"chain_of_thought"`) || strings.Contains(string(schema), `"thought"`) {
		t.Fatalf("schema=%s", schema)
	}
	other := validator.Schema()
	schema[0] = 'x'
	if other[0] != '{' {
		t.Fatal("Schema exposed mutable package bytes")
	}
	valid := json.RawMessage(`{"schema_version":1,"type":"TOOL","reason_code":"CALL","tool":{"name":"crm.lookup","arguments":{}}}`)
	if err := validator.Validate(other, valid); err != nil {
		t.Fatal(err)
	}
	modified := append(json.RawMessage(nil), other...)
	modified = append(modified[:len(modified)-1], []byte(`,"unexpected":true}`)...)
	if err := validator.Validate(modified, valid); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("modified schema error=%v", err)
	}
}

func TestActionFieldSizeBoundaries(t *testing.T) {
	query := strings.Repeat("q", 8<<10)
	search := `{"schema_version":1,"type":"SEARCH","reason_code":"SEARCH","search":{"query":"` + query + `","top_k":100,"knowledge_base_ids":["kb_0001"]}}`
	if _, err := Decode(json.RawMessage(search)); err != nil {
		t.Fatal(err)
	}
	tooLong := strings.Repeat("q", (8<<10)+1)
	search = `{"schema_version":1,"type":"SEARCH","reason_code":"SEARCH","search":{"query":"` + tooLong + `","top_k":100,"knowledge_base_ids":["kb_0001"]}}`
	if _, err := Decode(json.RawMessage(search)); !errors.Is(err, ErrInvalidAction) {
		t.Fatalf("query limit error=%v", err)
	}
	if _, err := (Action{}).Encode(); !errors.Is(err, ErrInvalidAction) {
		t.Fatalf("zero Action error=%v", err)
	}
	if _, err := NewParser(nil, Config{MaximumRawBytes: MaximumRawSize + 1}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("parser config error=%v", err)
	}
}
