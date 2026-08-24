package rag

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestReferenceGoldenSetPassesDigestBoundBaseline(t *testing.T) {
	dataset, predictions, baseline := loadReference(t)
	report, err := Evaluate(dataset, predictions)
	if err != nil {
		t.Fatal(err)
	}
	report, err = ApplyBaseline(report, baseline)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Passed || report.CaseCount != 10 || report.HumanReviewedCases != 3 || report.TopK != 5 ||
		report.Metrics != (Metrics{RecallAtK: 1, MRRAtK: 1, CitationCorrectness: 1, Faithfulness: 1}) || len(report.Violations) != 0 {
		t.Fatalf("reference report = %+v", report)
	}
}

func TestRegressionGateReportsStableViolations(t *testing.T) {
	dataset, predictions, baseline := loadReference(t)
	predictions.Predictions[0].RankedChunkIDs = []string{"chk_noise_9999"}
	predictions.Predictions[0].CitedArtifactIDs = []string{"art_noise_9999"}
	predictions.Predictions[0].Answer = "退款会当天到账。"
	report, err := Evaluate(dataset, predictions)
	if err != nil {
		t.Fatal(err)
	}
	report, err = ApplyBaseline(report, baseline)
	if !errors.Is(err, ErrGateFailed) || report.Passed || len(report.Violations) != 2 {
		t.Fatalf("regression report=%+v error=%v", report, err)
	}
	want := []string{
		"faithfulness 0.900000 below minimum 0.950000",
		"faithfulness 0.900000 below regression floor 0.950000",
	}
	if !slices.Equal(report.Violations, want) {
		t.Fatalf("violations=%v want=%v", report.Violations, want)
	}
}

func TestMetricDefinitionsPenalizeRankMissingAndInventedEvidence(t *testing.T) {
	recall, mrr := retrievalMetrics([]string{"chk_a0001", "chk_b0001"},
		[]string{"chk_noise1", "chk_a0001", "chk_b0001"}, 2)
	if recall != 0.5 || mrr != 0.5 {
		t.Fatalf("retrieval metrics recall=%v mrr=%v", recall, mrr)
	}
	if score := setF1([]string{"art_a0001", "art_b0001"}, []string{"art_a0001", "art_fake1"}); score != 0.5 {
		t.Fatalf("citation F1=%v", score)
	}
	golden := GoldenCase{Claims: []GoldenClaim{{MatchAny: []string{"supported phrase"}, SupportingArtifactIDs: []string{"art_a0001"}}},
		Contradictions: []string{"false phrase"}}
	if score := faithfulness(golden, Prediction{Answer: "supported phrase and false phrase", CitedArtifactIDs: []string{"art_a0001"}}); score != 0.5 {
		t.Fatalf("faithfulness=%v", score)
	}
	if score := faithfulness(golden, Prediction{Answer: "unmatched answer"}); score != 0 {
		t.Fatalf("vacuous faithfulness=%v", score)
	}
}

func TestPredictionOrderDoesNotChangeMacroMetrics(t *testing.T) {
	dataset, predictions, _ := loadReference(t)
	first, err := Evaluate(dataset, predictions)
	if err != nil {
		t.Fatal(err)
	}
	slices.Reverse(predictions.Predictions)
	second, err := Evaluate(dataset, predictions)
	if err != nil {
		t.Fatal(err)
	}
	if first.Metrics != second.Metrics {
		t.Fatalf("order changed metrics: first=%+v second=%+v", first.Metrics, second.Metrics)
	}
}

func TestStrictLoadersRejectDriftAndWeakHumanSlice(t *testing.T) {
	datasetData := readFixture(t, filepath.Join("golden", "v1", "dataset.json"))
	var raw map[string]any
	if err := json.Unmarshal(datasetData, &raw); err != nil {
		t.Fatal(err)
	}
	raw["unknown"] = true
	unknown, _ := json.Marshal(raw)
	if _, err := LoadDataset(unknown); !errors.Is(err, ErrInvalidData) {
		t.Fatalf("unknown dataset field error=%v", err)
	}
	duplicate := []byte(strings.Replace(string(datasetData), `"schema_version": 1`, `"schema_version": 1, "schema_version": 1`, 1))
	if _, err := LoadDataset(duplicate); !errors.Is(err, ErrInvalidData) {
		t.Fatalf("duplicate JSON field error=%v", err)
	}
	delete(raw, "unknown")
	cases := raw["cases"].([]any)
	for _, value := range cases {
		item := value.(map[string]any)
		item["human_reviewed"] = false
		delete(item, "review_note")
	}
	weak, _ := json.Marshal(raw)
	if _, err := LoadDataset(weak); !errors.Is(err, ErrInvalidData) || !strings.Contains(err.Error(), "20%") {
		t.Fatalf("weak human slice error=%v", err)
	}
}

func TestBaselineRejectsDifferentGoldenDigest(t *testing.T) {
	dataset, predictions, baseline := loadReference(t)
	report, err := Evaluate(dataset, predictions)
	if err != nil {
		t.Fatal(err)
	}
	baseline.DatasetSHA256 = strings.Repeat("0", 64)
	if _, err := ApplyBaseline(report, baseline); !errors.Is(err, ErrInvalidData) {
		t.Fatalf("digest mismatch error=%v", err)
	}
}

func loadReference(t *testing.T) (Dataset, PredictionSet, Baseline) {
	t.Helper()
	dataset, err := LoadDataset(readFixture(t, filepath.Join("golden", "v1", "dataset.json")))
	if err != nil {
		t.Fatal(err)
	}
	predictions, err := LoadPredictions(readFixture(t, filepath.Join("predictions", "reference-v1.json")), dataset)
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := LoadBaseline(readFixture(t, filepath.Join("baselines", "v1.json")))
	if err != nil {
		t.Fatal(err)
	}
	return dataset, predictions, baseline
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	value, err := os.ReadFile(filepath.Join("..", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return value
}
