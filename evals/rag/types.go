// Package rag implements deterministic offline RAG quality evaluation.
package rag

import (
	"crypto/sha256"
	"encoding/hex"
)

const SchemaVersion = 1

type Dataset struct {
	SchemaVersion int          `json:"schema_version"`
	DatasetID     string       `json:"dataset_id"`
	Revision      string       `json:"revision"`
	TopK          int          `json:"top_k"`
	Cases         []GoldenCase `json:"cases"`
	digest        [sha256.Size]byte
}

type GoldenCase struct {
	ID                  string        `json:"id"`
	Query               string        `json:"query"`
	RelevantChunkIDs    []string      `json:"relevant_chunk_ids"`
	ExpectedCitationIDs []string      `json:"expected_citation_ids"`
	Claims              []GoldenClaim `json:"claims"`
	Contradictions      []string      `json:"contradictions"`
	Provenance          string        `json:"provenance"`
	HumanReviewed       bool          `json:"human_reviewed"`
	ReviewNote          string        `json:"review_note,omitempty"`
}

type GoldenClaim struct {
	ID                    string   `json:"id"`
	MatchAny              []string `json:"match_any"`
	SupportingArtifactIDs []string `json:"supporting_artifact_ids"`
}

type PredictionSet struct {
	SchemaVersion int          `json:"schema_version"`
	DatasetID     string       `json:"dataset_id"`
	Predictions   []Prediction `json:"predictions"`
}

type Prediction struct {
	CaseID           string   `json:"case_id"`
	RankedChunkIDs   []string `json:"ranked_chunk_ids"`
	CitedArtifactIDs []string `json:"cited_artifact_ids"`
	Answer           string   `json:"answer"`
}

type Metrics struct {
	RecallAtK           float64 `json:"recall_at_k"`
	MRRAtK              float64 `json:"mrr_at_k"`
	CitationCorrectness float64 `json:"citation_correctness"`
	Faithfulness        float64 `json:"faithfulness"`
}

type Report struct {
	SchemaVersion      int      `json:"schema_version"`
	DatasetID          string   `json:"dataset_id"`
	DatasetRevision    string   `json:"dataset_revision"`
	DatasetSHA256      string   `json:"dataset_sha256"`
	TopK               int      `json:"top_k"`
	CaseCount          int      `json:"case_count"`
	HumanReviewedCases int      `json:"human_reviewed_cases"`
	Metrics            Metrics  `json:"metrics"`
	Passed             bool     `json:"passed"`
	Violations         []string `json:"violations"`
}

type Baseline struct {
	SchemaVersion     int     `json:"schema_version"`
	DatasetID         string  `json:"dataset_id"`
	DatasetSHA256     string  `json:"dataset_sha256"`
	TopK              int     `json:"top_k"`
	Reference         Metrics `json:"reference"`
	Minimum           Metrics `json:"minimum"`
	MaximumRegression Metrics `json:"maximum_regression"`
}

func (d Dataset) SHA256() string { return hex.EncodeToString(d.digest[:]) }
