// Package action defines the strict structured-output protocol for Agent decisions.
package action

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"regexp"
	"strings"
)

const (
	SchemaVersion  = 1
	MaximumRawSize = 1 << 20
)

var (
	ErrInvalidArgument = errors.New("agent action invalid argument")
	ErrInvalidAction   = errors.New("agent action rejected")
)

type FailureCode string

const (
	FailureOversized FailureCode = "OVERSIZED"
	FailureSyntax    FailureCode = "INVALID_JSON"
	FailureContract  FailureCode = "CONTRACT_VIOLATION"
	FailureRepair    FailureCode = "REPAIR_FAILED"
)

type ParseError struct {
	code  FailureCode
	cause error
}

func (e *ParseError) Error() string {
	if e == nil {
		return ErrInvalidAction.Error()
	}
	return ErrInvalidAction.Error() + ": " + string(e.code)
}
func (e *ParseError) Unwrap() error {
	if e == nil {
		return nil
	}
	return errors.Join(ErrInvalidAction, e.cause)
}
func (e *ParseError) Code() FailureCode {
	if e == nil {
		return FailureContract
	}
	return e.code
}

type Type string

const (
	TypeSearch Type = "SEARCH"
	TypeTool   Type = "TOOL"
	TypeFinal  Type = "FINAL"
)

type Search struct {
	Query            string
	TopK             int
	KnowledgeBaseIDs []string
	Filters          json.RawMessage
}

type Tool struct {
	Name      string
	Arguments json.RawMessage
}

type Final struct {
	Answer      string
	CitationIDs []ArtifactID
}

type ArtifactID string

func (id ArtifactID) String() string { return string(id) }

type Action struct {
	actionType Type
	reasonCode string
	search     *Search
	tool       *Tool
	final      *Final
}

func (a Action) Type() Type         { return a.actionType }
func (a Action) ReasonCode() string { return a.reasonCode }
func (a Action) Search() (Search, bool) {
	if a.search == nil {
		return Search{}, false
	}
	return cloneSearch(*a.search), true
}
func (a Action) Tool() (Tool, bool) {
	if a.tool == nil {
		return Tool{}, false
	}
	return cloneTool(*a.tool), true
}
func (a Action) Final() (Final, bool) {
	if a.final == nil {
		return Final{}, false
	}
	return cloneFinal(*a.final), true
}

func (a Action) MarshalJSON() ([]byte, error) {
	wire := wireAction{SchemaVersion: SchemaVersion, Type: a.actionType, ReasonCode: a.reasonCode}
	switch a.actionType {
	case TypeSearch:
		if a.search == nil {
			return nil, contractError("invalid SEARCH Action")
		}
		value := cloneSearch(*a.search)
		wire.Search = &wireSearch{Query: value.Query, TopK: value.TopK,
			KnowledgeBaseIDs: value.KnowledgeBaseIDs, Filters: value.Filters}
	case TypeTool:
		if a.tool == nil {
			return nil, contractError("invalid TOOL Action")
		}
		value := cloneTool(*a.tool)
		wire.Tool = &wireTool{Name: value.Name, Arguments: value.Arguments}
	case TypeFinal:
		if a.final == nil {
			return nil, contractError("invalid FINAL Action")
		}
		value := cloneFinal(*a.final)
		citations := make([]string, len(value.CitationIDs))
		for index, identifier := range value.CitationIDs {
			citations[index] = identifier.String()
		}
		wire.Final = &wireFinal{Answer: value.Answer, CitationIDs: citations}
	default:
		return nil, contractError("invalid Action type")
	}
	if _, err := wire.toAction(); err != nil {
		return nil, err
	}
	return json.Marshal(wire)
}

func (a Action) Encode() (json.RawMessage, error) {
	value, err := a.MarshalJSON()
	return json.RawMessage(value), err
}

type RepairRequest struct {
	InvalidJSON json.RawMessage
	FailureCode FailureCode
}

type Repairer interface {
	Repair(context.Context, RepairRequest) (json.RawMessage, error)
}

type RepairerFunc func(context.Context, RepairRequest) (json.RawMessage, error)

func (f RepairerFunc) Repair(ctx context.Context, request RepairRequest) (json.RawMessage, error) {
	return f(ctx, request)
}

type Parser struct {
	repairer Repairer
	maximum  int
}

type Config struct{ MaximumRawBytes int }

func DefaultConfig() Config { return Config{MaximumRawBytes: MaximumRawSize} }

func NewParser(repairer Repairer, configuration Config) (*Parser, error) {
	if configuration.MaximumRawBytes < 1 || configuration.MaximumRawBytes > MaximumRawSize {
		return nil, fmt.Errorf("%w: maximum raw bytes", ErrInvalidArgument)
	}
	return &Parser{repairer: repairer, maximum: configuration.MaximumRawBytes}, nil
}

type Result struct {
	Action   Action
	Repaired bool
}

func (p *Parser) Parse(ctx context.Context, raw json.RawMessage) (Result, error) {
	if ctx == nil || p == nil {
		return Result{}, fmt.Errorf("%w: parser or context", ErrInvalidArgument)
	}
	value, err := decode(raw, p.maximum)
	if err == nil {
		return Result{Action: value}, nil
	}
	parseErr := asParseError(err)
	if p.repairer == nil || parseErr.Code() == FailureOversized || ctx.Err() != nil {
		return Result{}, err
	}
	repaired, repairErr := p.repairer.Repair(ctx, RepairRequest{InvalidJSON: cloneJSON(raw), FailureCode: parseErr.Code()})
	if repairErr != nil {
		return Result{}, &ParseError{code: FailureRepair, cause: repairErr}
	}
	value, err = decode(repaired, p.maximum)
	if err != nil {
		return Result{}, &ParseError{code: FailureRepair, cause: err}
	}
	return Result{Action: value, Repaired: true}, nil
}

func Decode(raw json.RawMessage) (Action, error) { return decode(raw, MaximumRawSize) }

type wireAction struct {
	SchemaVersion int         `json:"schema_version"`
	Type          Type        `json:"type"`
	ReasonCode    string      `json:"reason_code"`
	Search        *wireSearch `json:"search,omitempty"`
	Tool          *wireTool   `json:"tool,omitempty"`
	Final         *wireFinal  `json:"final,omitempty"`
}

type wireSearch struct {
	Query            string          `json:"query"`
	TopK             int             `json:"top_k"`
	KnowledgeBaseIDs []string        `json:"knowledge_base_ids"`
	Filters          json.RawMessage `json:"filters,omitempty"`
}

type wireTool struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type wireFinal struct {
	Answer      string   `json:"answer"`
	CitationIDs []string `json:"citation_ids"`
}

var (
	reasonCodePattern      = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)
	knowledgeBaseIDPattern = regexp.MustCompile(`^kb_[A-Za-z0-9_-]{4,124}$`)
	toolNamePattern        = regexp.MustCompile(`^[a-z][a-z0-9]*(?:\.[a-z][a-z0-9_-]*)+$`)
	artifactIDPattern      = regexp.MustCompile(`^art_[A-Za-z0-9_-]{4,124}$`)
)

func decode(raw json.RawMessage, maximum int) (Action, error) {
	if len(raw) > maximum {
		return Action{}, &ParseError{code: FailureOversized}
	}
	if len(raw) == 0 {
		return Action{}, &ParseError{code: FailureSyntax}
	}
	if err := validateJSONShape(raw); err != nil {
		return Action{}, &ParseError{code: FailureSyntax, cause: err}
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var wire wireAction
	if err := decoder.Decode(&wire); err != nil {
		return Action{}, &ParseError{code: FailureContract, cause: err}
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return Action{}, &ParseError{code: FailureSyntax, cause: err}
	}
	action, err := wire.toAction()
	if err != nil {
		return Action{}, err
	}
	canonical, err := action.MarshalJSON()
	if err != nil || !sameJSON(raw, canonical) {
		return Action{}, contractError("non-canonical field names or required fields")
	}
	return action, nil
}

func (w wireAction) toAction() (Action, error) {
	if w.SchemaVersion != SchemaVersion || !reasonCodePattern.MatchString(w.ReasonCode) {
		return Action{}, contractError("header")
	}
	payloads := 0
	if w.Search != nil {
		payloads++
	}
	if w.Tool != nil {
		payloads++
	}
	if w.Final != nil {
		payloads++
	}
	if payloads != 1 {
		return Action{}, contractError("payload cardinality")
	}
	switch w.Type {
	case TypeSearch:
		if w.Search == nil {
			return Action{}, contractError("SEARCH discriminator")
		}
		value, err := w.Search.validate()
		if err != nil {
			return Action{}, err
		}
		return Action{actionType: w.Type, reasonCode: w.ReasonCode, search: &value}, nil
	case TypeTool:
		if w.Tool == nil {
			return Action{}, contractError("TOOL discriminator")
		}
		value, err := w.Tool.validate()
		if err != nil {
			return Action{}, err
		}
		return Action{actionType: w.Type, reasonCode: w.ReasonCode, tool: &value}, nil
	case TypeFinal:
		if w.Final == nil {
			return Action{}, contractError("FINAL discriminator")
		}
		value, err := w.Final.validate()
		if err != nil {
			return Action{}, err
		}
		return Action{actionType: w.Type, reasonCode: w.ReasonCode, final: &value}, nil
	default:
		return Action{}, contractError("type")
	}
}

func (w wireSearch) validate() (Search, error) {
	if strings.TrimSpace(w.Query) != w.Query || w.Query == "" || len(w.Query) > 8<<10 || w.TopK < 1 || w.TopK > 100 ||
		len(w.KnowledgeBaseIDs) == 0 || len(w.KnowledgeBaseIDs) > 32 {
		return Search{}, contractError("SEARCH limits")
	}
	seen := make(map[string]struct{}, len(w.KnowledgeBaseIDs))
	for _, id := range w.KnowledgeBaseIDs {
		if !knowledgeBaseIDPattern.MatchString(id) {
			return Search{}, contractError("knowledge base ID")
		}
		if _, duplicate := seen[id]; duplicate {
			return Search{}, contractError("duplicate knowledge base ID")
		}
		seen[id] = struct{}{}
	}
	if len(w.Filters) > 32<<10 || (len(w.Filters) > 0 && !jsonObject(w.Filters)) {
		return Search{}, contractError("SEARCH filters")
	}
	return Search{Query: w.Query, TopK: w.TopK, KnowledgeBaseIDs: append([]string(nil), w.KnowledgeBaseIDs...), Filters: cloneJSON(w.Filters)}, nil
}

func (w wireTool) validate() (Tool, error) {
	if !toolNamePattern.MatchString(w.Name) || len(w.Name) > 128 || len(w.Arguments) == 0 || len(w.Arguments) > 64<<10 || !jsonObject(w.Arguments) {
		return Tool{}, contractError("TOOL payload")
	}
	return Tool{Name: w.Name, Arguments: cloneJSON(w.Arguments)}, nil
}

func (w wireFinal) validate() (Final, error) {
	if strings.TrimSpace(w.Answer) != w.Answer || w.Answer == "" || len(w.Answer) > 1<<20 || len(w.CitationIDs) > 256 {
		return Final{}, contractError("FINAL limits")
	}
	identifiers := make([]ArtifactID, len(w.CitationIDs))
	seen := make(map[ArtifactID]struct{}, len(w.CitationIDs))
	for index, raw := range w.CitationIDs {
		if !artifactIDPattern.MatchString(raw) {
			return Final{}, contractError("citation ID")
		}
		identifier := ArtifactID(raw)
		if _, duplicate := seen[identifier]; duplicate {
			return Final{}, contractError("duplicate citation ID")
		}
		seen[identifier], identifiers[index] = struct{}{}, identifier
	}
	return Final{Answer: w.Answer, CitationIDs: identifiers}, nil
}

func validateJSONShape(raw json.RawMessage) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := walkJSON(decoder); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON value")
	}
	return nil
}

func walkJSON(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, composite := token.(json.Delim)
	if !composite {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("object key is not a string")
			}
			if _, duplicate := seen[key]; duplicate {
				return errors.New("duplicate object key")
			}
			seen[key] = struct{}{}
			if err := walkJSON(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim('}') {
			return errors.New("invalid object close")
		}
	case '[':
		for decoder.More() {
			if err := walkJSON(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim(']') {
			return errors.New("invalid array close")
		}
	default:
		return errors.New("invalid JSON delimiter")
	}
	return nil
}

func jsonObject(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) >= 2 && trimmed[0] == '{' && trimmed[len(trimmed)-1] == '}' && json.Valid(trimmed)
}

func contractError(reason string) error {
	return &ParseError{code: FailureContract, cause: errors.New(reason)}
}

func asParseError(err error) *ParseError {
	var value *ParseError
	if errors.As(err, &value) {
		return value
	}
	return &ParseError{code: FailureContract, cause: err}
}

func cloneSearch(value Search) Search {
	value.KnowledgeBaseIDs = append([]string(nil), value.KnowledgeBaseIDs...)
	value.Filters = cloneJSON(value.Filters)
	return value
}
func cloneTool(value Tool) Tool {
	value.Arguments = cloneJSON(value.Arguments)
	return value
}
func cloneFinal(value Final) Final {
	value.CitationIDs = append([]ArtifactID(nil), value.CitationIDs...)
	return value
}
func cloneJSON(value json.RawMessage) json.RawMessage { return append(json.RawMessage(nil), value...) }

type Validator struct{ schema json.RawMessage }

func NewValidator() *Validator { return &Validator{schema: json.RawMessage(actionSchema)} }
func (v *Validator) Schema() json.RawMessage {
	if v == nil {
		return nil
	}
	return cloneJSON(v.schema)
}
func (v *Validator) Validate(schema, value json.RawMessage) error {
	if v == nil || !sameJSON(schema, v.schema) {
		return fmt.Errorf("%w: unsupported Action schema", ErrInvalidArgument)
	}
	_, err := Decode(value)
	return err
}

func sameJSON(left, right json.RawMessage) bool {
	if validateJSONShape(left) != nil || validateJSONShape(right) != nil {
		return false
	}
	var leftValue, rightValue any
	if json.Unmarshal(left, &leftValue) != nil || json.Unmarshal(right, &rightValue) != nil {
		return false
	}
	return reflect.DeepEqual(leftValue, rightValue)
}

const actionSchema = `{
  "$schema":"https://json-schema.org/draft/2020-12/schema",
  "$id":"https://agenticrag.dev/schemas/agent-action-v1.json",
  "title":"Agent Action v1",
  "oneOf":[
    {"$ref":"#/$defs/searchAction"},
    {"$ref":"#/$defs/toolAction"},
    {"$ref":"#/$defs/finalAction"}
  ],
  "$defs":{
    "header":{
      "type":"object",
      "properties":{
        "schema_version":{"const":1},
        "reason_code":{"type":"string","pattern":"^[A-Z][A-Z0-9_]{0,63}$"}
      },
      "required":["schema_version","reason_code"]
    },
    "searchAction":{
      "type":"object","additionalProperties":false,
      "properties":{
        "schema_version":{"const":1},"type":{"const":"SEARCH"},
        "reason_code":{"type":"string","pattern":"^[A-Z][A-Z0-9_]{0,63}$"},
        "search":{"type":"object","additionalProperties":false,
          "properties":{"query":{"type":"string","minLength":1,"maxLength":8192},
            "top_k":{"type":"integer","minimum":1,"maximum":100},
            "knowledge_base_ids":{"type":"array","minItems":1,"maxItems":32,"uniqueItems":true,
              "items":{"type":"string","pattern":"^kb_[A-Za-z0-9_-]{4,124}$"}},
            "filters":{"type":"object"}},
          "required":["query","top_k","knowledge_base_ids"]}
      },"required":["schema_version","type","reason_code","search"]
    },
    "toolAction":{
      "type":"object","additionalProperties":false,
      "properties":{
        "schema_version":{"const":1},"type":{"const":"TOOL"},
        "reason_code":{"type":"string","pattern":"^[A-Z][A-Z0-9_]{0,63}$"},
        "tool":{"type":"object","additionalProperties":false,
          "properties":{"name":{"type":"string","pattern":"^[a-z][a-z0-9]*(?:\\.[a-z][a-z0-9_-]*)+$","maxLength":128},
            "arguments":{"type":"object"}},"required":["name","arguments"]}
      },"required":["schema_version","type","reason_code","tool"]
    },
    "finalAction":{
      "type":"object","additionalProperties":false,
      "properties":{
        "schema_version":{"const":1},"type":{"const":"FINAL"},
        "reason_code":{"type":"string","pattern":"^[A-Z][A-Z0-9_]{0,63}$"},
        "final":{"type":"object","additionalProperties":false,
          "properties":{"answer":{"type":"string","minLength":1,"maxLength":1048576},
            "citation_ids":{"type":"array","maxItems":256,"uniqueItems":true,
              "items":{"type":"string","pattern":"^art_[A-Za-z0-9_-]{4,124}$"}}},
          "required":["answer","citation_ids"]}
      },"required":["schema_version","type","reason_code","final"]
    }
  }
}`
