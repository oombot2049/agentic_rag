// Package modes provides versioned states and deterministic coordinators for
// the supported Agent execution modes.
package modes

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"

	"github.com/agenticrag/agenticrag/internal/agent/action"
)

const SchemaVersion = 1

var ErrInvalidState = errors.New("agent mode state is invalid")

var artifactPattern = regexp.MustCompile(`^art_[A-Za-z0-9_-]{4,124}$`)
var knowledgeBasePattern = regexp.MustCompile(`^kb_[A-Za-z0-9_-]{4,124}$`)

type Mode string

const (
	ModeDeterministicRAG Mode = "DETERMINISTIC_RAG"
	ModeReAct            Mode = "REACT"
)

type DeterministicStage string

const (
	StageRewrite  DeterministicStage = "QUERY_REWRITE"
	StageRetrieve DeterministicStage = "RETRIEVE"
	StageGenerate DeterministicStage = "GENERATE"
	StageDone     DeterministicStage = "DONE"
)

type DeterministicState struct {
	Stage            DeterministicStage  `json:"stage"`
	OriginalQuestion string              `json:"original_question"`
	ContextArtifact  action.ArtifactID   `json:"context_artifact_id,omitempty"`
	RewrittenQuery   string              `json:"rewritten_query,omitempty"`
	EvidenceIDs      []action.ArtifactID `json:"evidence_ids"`
	AnswerArtifact   action.ArtifactID   `json:"answer_artifact_id,omitempty"`
}

type ReActStage string

const (
	StageDecide ReActStage = "DECIDE"
	StageAct    ReActStage = "ACT"
	StageFinish ReActStage = "DONE"
)

type SearchRequest struct {
	Query            string          `json:"query"`
	TopK             int             `json:"top_k"`
	KnowledgeBaseIDs []string        `json:"knowledge_base_ids"`
	Filters          json.RawMessage `json:"filters,omitempty"`
}

type Observation struct {
	QueryFingerprint string              `json:"query_fingerprint"`
	EvidenceIDs      []action.ArtifactID `json:"evidence_ids"`
}

type ReActState struct {
	Stage            ReActStage          `json:"stage"`
	OriginalQuestion string              `json:"original_question"`
	ContextArtifact  action.ArtifactID   `json:"context_artifact_id,omitempty"`
	Iteration        int                 `json:"iteration"`
	MaxIterations    int                 `json:"max_iterations"`
	PendingSearch    *SearchRequest      `json:"pending_search,omitempty"`
	Observations     []Observation       `json:"observations"`
	AnswerArtifact   action.ArtifactID   `json:"answer_artifact_id,omitempty"`
	CitationIDs      []action.ArtifactID `json:"citation_ids"`
}

type Envelope struct {
	SchemaVersion int                 `json:"schema_version"`
	Mode          Mode                `json:"mode"`
	Deterministic *DeterministicState `json:"deterministic,omitempty"`
	ReAct         *ReActState         `json:"react,omitempty"`
}

func NewDeterministic(question string, context action.ArtifactID) (json.RawMessage, error) {
	return Encode(Envelope{SchemaVersion: SchemaVersion, Mode: ModeDeterministicRAG,
		Deterministic: &DeterministicState{Stage: StageRewrite, OriginalQuestion: question,
			ContextArtifact: context, EvidenceIDs: []action.ArtifactID{}}})
}

func NewReAct(question string, context action.ArtifactID, maxIterations int) (json.RawMessage, error) {
	return Encode(Envelope{SchemaVersion: SchemaVersion, Mode: ModeReAct,
		ReAct: &ReActState{Stage: StageDecide, OriginalQuestion: question, ContextArtifact: context,
			MaxIterations: maxIterations, Observations: []Observation{}, CitationIDs: []action.ArtifactID{}}})
}

func Decode(raw json.RawMessage) (Envelope, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var value Envelope
	if len(raw) == 0 || decoder.Decode(&value) != nil {
		return Envelope{}, ErrInvalidState
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return Envelope{}, ErrInvalidState
	}
	if err := validate(value); err != nil {
		return Envelope{}, err
	}
	return cloneEnvelope(value), nil
}

func Encode(value Envelope) (json.RawMessage, error) {
	if err := validate(value); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("%w: encode", ErrInvalidState)
	}
	return encoded, nil
}

func ApplyRewrite(raw json.RawMessage, rewritten string) (json.RawMessage, error) {
	value, err := Decode(raw)
	if err != nil || value.Mode != ModeDeterministicRAG || value.Deterministic.Stage != StageRewrite || !validText(rewritten, 8192) {
		return nil, ErrInvalidState
	}
	value.Deterministic.RewrittenQuery = rewritten
	value.Deterministic.Stage = StageRetrieve
	return Encode(value)
}

func ApplyEvidence(raw json.RawMessage, evidence []action.ArtifactID) (json.RawMessage, error) {
	value, err := Decode(raw)
	if err != nil || value.Mode != ModeDeterministicRAG || value.Deterministic.Stage != StageRetrieve || !validIDs(evidence, 256) {
		return nil, ErrInvalidState
	}
	value.Deterministic.EvidenceIDs = append([]action.ArtifactID(nil), evidence...)
	value.Deterministic.Stage = StageGenerate
	return Encode(value)
}

func ApplyAnswer(raw json.RawMessage, artifact action.ArtifactID) (json.RawMessage, error) {
	value, err := Decode(raw)
	if err != nil || value.Mode != ModeDeterministicRAG || value.Deterministic.Stage != StageGenerate || !validArtifact(artifact) {
		return nil, ErrInvalidState
	}
	value.Deterministic.AnswerArtifact = artifact
	value.Deterministic.Stage = StageDone
	return Encode(value)
}

func ApplyReActDecision(raw json.RawMessage, decision action.Action, answerArtifact action.ArtifactID) (json.RawMessage, error) {
	value, err := Decode(raw)
	if err != nil || value.Mode != ModeReAct || value.ReAct.Stage != StageDecide || value.ReAct.Iteration >= value.ReAct.MaxIterations {
		return nil, ErrInvalidState
	}
	value.ReAct.Iteration++
	switch decision.Type() {
	case action.TypeSearch:
		if value.ReAct.Iteration >= value.ReAct.MaxIterations {
			return nil, fmt.Errorf("%w: final iteration must finish", ErrInvalidState)
		}
		search, _ := decision.Search()
		request := SearchRequest{Query: search.Query, TopK: search.TopK, KnowledgeBaseIDs: search.KnowledgeBaseIDs, Filters: search.Filters}
		if duplicateSearch(value.ReAct.Observations, request) {
			return nil, fmt.Errorf("%w: duplicate search", ErrInvalidState)
		}
		value.ReAct.PendingSearch = &request
		value.ReAct.Stage = StageAct
	case action.TypeFinal:
		final, _ := decision.Final()
		if !validArtifact(answerArtifact) || !citationsObserved(final.CitationIDs, value.ReAct.Observations) {
			return nil, ErrInvalidState
		}
		value.ReAct.AnswerArtifact = answerArtifact
		value.ReAct.CitationIDs = append([]action.ArtifactID(nil), final.CitationIDs...)
		value.ReAct.Stage = StageFinish
	default:
		return nil, fmt.Errorf("%w: only SEARCH or FINAL is supported", ErrInvalidState)
	}
	return Encode(value)
}

func ApplyObservation(raw json.RawMessage, evidence []action.ArtifactID) (json.RawMessage, error) {
	value, err := Decode(raw)
	if err != nil || value.Mode != ModeReAct || value.ReAct.Stage != StageAct || value.ReAct.PendingSearch == nil || !validIDs(evidence, 256) {
		return nil, ErrInvalidState
	}
	fingerprint, err := searchFingerprint(*value.ReAct.PendingSearch)
	if err != nil {
		return nil, err
	}
	value.ReAct.Observations = append(value.ReAct.Observations, Observation{QueryFingerprint: fingerprint,
		EvidenceIDs: append([]action.ArtifactID(nil), evidence...)})
	value.ReAct.PendingSearch = nil
	value.ReAct.Stage = StageDecide
	return Encode(value)
}

func validate(value Envelope) error {
	if value.SchemaVersion != SchemaVersion || !validQuestion(selectQuestion(value)) {
		return ErrInvalidState
	}
	switch value.Mode {
	case ModeDeterministicRAG:
		if value.Deterministic == nil || value.ReAct != nil {
			return ErrInvalidState
		}
		return validateDeterministic(*value.Deterministic)
	case ModeReAct:
		if value.ReAct == nil || value.Deterministic != nil {
			return ErrInvalidState
		}
		return validateReAct(*value.ReAct)
	default:
		return ErrInvalidState
	}
}

func validateDeterministic(value DeterministicState) error {
	if !validOptionalArtifact(value.ContextArtifact) || !validIDs(value.EvidenceIDs, 256) {
		return ErrInvalidState
	}
	switch value.Stage {
	case StageRewrite:
		if value.RewrittenQuery != "" || len(value.EvidenceIDs) != 0 || value.AnswerArtifact != "" {
			return ErrInvalidState
		}
	case StageRetrieve:
		if !validText(value.RewrittenQuery, 8192) || len(value.EvidenceIDs) != 0 || value.AnswerArtifact != "" {
			return ErrInvalidState
		}
	case StageGenerate:
		if !validText(value.RewrittenQuery, 8192) || value.AnswerArtifact != "" {
			return ErrInvalidState
		}
	case StageDone:
		if !validText(value.RewrittenQuery, 8192) || !validArtifact(value.AnswerArtifact) {
			return ErrInvalidState
		}
	default:
		return ErrInvalidState
	}
	return nil
}

func validateReAct(value ReActState) error {
	if !validOptionalArtifact(value.ContextArtifact) || value.MaxIterations < 1 || value.MaxIterations > 20 ||
		value.Iteration < 0 || value.Iteration > value.MaxIterations || len(value.Observations) > value.MaxIterations ||
		!validIDs(value.CitationIDs, 256) {
		return ErrInvalidState
	}
	seen := map[string]struct{}{}
	for _, observation := range value.Observations {
		if len(observation.QueryFingerprint) != 64 || !validIDs(observation.EvidenceIDs, 256) {
			return ErrInvalidState
		}
		if _, ok := seen[observation.QueryFingerprint]; ok {
			return ErrInvalidState
		}
		seen[observation.QueryFingerprint] = struct{}{}
	}
	switch value.Stage {
	case StageDecide:
		if value.PendingSearch != nil || value.AnswerArtifact != "" || len(value.CitationIDs) != 0 || value.Iteration >= value.MaxIterations {
			return ErrInvalidState
		}
	case StageAct:
		if value.PendingSearch == nil || value.AnswerArtifact != "" || len(value.CitationIDs) != 0 {
			return ErrInvalidState
		}
		if _, err := searchFingerprint(*value.PendingSearch); err != nil {
			return err
		}
	case StageFinish:
		if value.PendingSearch != nil || !validArtifact(value.AnswerArtifact) || !citationsObserved(value.CitationIDs, value.Observations) {
			return ErrInvalidState
		}
	default:
		return ErrInvalidState
	}
	return nil
}

func searchFingerprint(value SearchRequest) (string, error) {
	if !validText(value.Query, 8192) || value.TopK < 1 || value.TopK > 100 || len(value.KnowledgeBaseIDs) < 1 || len(value.KnowledgeBaseIDs) > 32 ||
		!validKnowledgeBases(value.KnowledgeBaseIDs) {
		return "", ErrInvalidState
	}
	var filters map[string]any
	if len(value.Filters) > 0 {
		decoder := json.NewDecoder(bytes.NewReader(value.Filters))
		decoder.UseNumber()
		if decoder.Decode(&filters) != nil || filters == nil {
			return "", ErrInvalidState
		}
		var trailing any
		if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
			return "", ErrInvalidState
		}
	}
	knowledgeBaseIDs := append([]string(nil), value.KnowledgeBaseIDs...)
	slices.Sort(knowledgeBaseIDs)
	canonical := struct {
		Query            string         `json:"query"`
		TopK             int            `json:"top_k"`
		KnowledgeBaseIDs []string       `json:"knowledge_base_ids"`
		Filters          map[string]any `json:"filters,omitempty"`
	}{value.Query, value.TopK, knowledgeBaseIDs, filters}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return "", ErrInvalidState
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func duplicateSearch(values []Observation, request SearchRequest) bool {
	fingerprint, err := searchFingerprint(request)
	if err != nil {
		return true
	}
	for _, value := range values {
		if value.QueryFingerprint == fingerprint {
			return true
		}
	}
	return false
}

func citationsObserved(citations []action.ArtifactID, observations []Observation) bool {
	allowed := map[action.ArtifactID]struct{}{}
	for _, observation := range observations {
		for _, id := range observation.EvidenceIDs {
			allowed[id] = struct{}{}
		}
	}
	for _, id := range citations {
		if _, ok := allowed[id]; !ok {
			return false
		}
	}
	return validIDs(citations, 256)
}

func validQuestion(value string) bool { return validText(value, 8192) }
func validText(value string, maximum int) bool {
	return value != "" && len(value) <= maximum && strings.TrimSpace(value) == value
}
func validArtifact(value action.ArtifactID) bool         { return artifactPattern.MatchString(value.String()) }
func validOptionalArtifact(value action.ArtifactID) bool { return value == "" || validArtifact(value) }
func validIDs(values []action.ArtifactID, maximum int) bool {
	if values == nil || len(values) > maximum {
		return false
	}
	seen := map[action.ArtifactID]struct{}{}
	for _, value := range values {
		if !validArtifact(value) {
			return false
		}
		if _, ok := seen[value]; ok {
			return false
		}
		seen[value] = struct{}{}
	}
	return true
}
func validKnowledgeBases(values []string) bool {
	seen := map[string]struct{}{}
	for _, value := range values {
		if !knowledgeBasePattern.MatchString(value) {
			return false
		}
		if _, ok := seen[value]; ok {
			return false
		}
		seen[value] = struct{}{}
	}
	return true
}
func selectQuestion(value Envelope) string {
	if value.Deterministic != nil {
		return value.Deterministic.OriginalQuestion
	}
	if value.ReAct != nil {
		return value.ReAct.OriginalQuestion
	}
	return ""
}
func cloneEnvelope(value Envelope) Envelope {
	encoded, _ := json.Marshal(value)
	var result Envelope
	_ = json.Unmarshal(encoded, &result)
	return result
}
