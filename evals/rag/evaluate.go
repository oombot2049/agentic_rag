package rag

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"slices"
	"strings"
)

var ErrGateFailed = errors.New("RAG evaluation gate failed")

const comparisonTolerance = 1e-12

func Evaluate(dataset Dataset, predictions PredictionSet) (Report, error) {
	if err := validateDataset(dataset); err != nil {
		return Report{}, fmt.Errorf("%w: incompatible evaluation inputs", ErrInvalidData)
	}
	if err := validatePredictions(predictions, dataset); err != nil {
		return Report{}, err
	}
	byCase := make(map[string]Prediction, len(predictions.Predictions))
	for _, prediction := range predictions.Predictions {
		byCase[prediction.CaseID] = prediction
	}
	if len(byCase) != len(dataset.Cases) {
		return Report{}, fmt.Errorf("%w: missing or duplicate predictions", ErrInvalidData)
	}
	var metrics Metrics
	humanReviewed := 0
	for _, golden := range dataset.Cases {
		prediction, ok := byCase[golden.ID]
		if !ok {
			return Report{}, fmt.Errorf("%w: missing prediction %q", ErrInvalidData, golden.ID)
		}
		recall, reciprocalRank := retrievalMetrics(golden.RelevantChunkIDs, prediction.RankedChunkIDs, dataset.TopK)
		metrics.RecallAtK += recall
		metrics.MRRAtK += reciprocalRank
		metrics.CitationCorrectness += setF1(golden.ExpectedCitationIDs, prediction.CitedArtifactIDs)
		metrics.Faithfulness += faithfulness(golden, prediction)
		if golden.HumanReviewed {
			humanReviewed++
		}
	}
	count := float64(len(dataset.Cases))
	metrics.RecallAtK /= count
	metrics.MRRAtK /= count
	metrics.CitationCorrectness /= count
	metrics.Faithfulness /= count
	return Report{SchemaVersion: SchemaVersion, DatasetID: dataset.DatasetID, DatasetRevision: dataset.Revision,
		DatasetSHA256: dataset.SHA256(), TopK: dataset.TopK, CaseCount: len(dataset.Cases),
		HumanReviewedCases: humanReviewed, Metrics: metrics, Passed: true, Violations: []string{}}, nil
}

func ApplyBaseline(report Report, baseline Baseline) (Report, error) {
	digest, err := parseDigest(baseline.DatasetSHA256)
	reportDigest, reportDigestErr := parseDigest(report.DatasetSHA256)
	if err != nil || reportDigestErr != nil || validateBaseline(baseline) != nil || report.SchemaVersion != SchemaVersion ||
		report.DatasetID != baseline.DatasetID || report.TopK != baseline.TopK ||
		subtle.ConstantTimeCompare(digest[:], reportDigest[:]) != 1 {
		return Report{}, fmt.Errorf("%w: baseline is not bound to this dataset", ErrInvalidData)
	}
	violations := make([]string, 0, 8)
	check := func(name string, current, minimum, reference, maximumDrop float64) {
		if current+comparisonTolerance < minimum {
			violations = append(violations, fmt.Sprintf("%s %.6f below minimum %.6f", name, current, minimum))
		}
		regressionFloor := reference - maximumDrop
		if current+comparisonTolerance < regressionFloor {
			violations = append(violations, fmt.Sprintf("%s %.6f below regression floor %.6f", name, current, regressionFloor))
		}
	}
	check("citation_correctness", report.Metrics.CitationCorrectness, baseline.Minimum.CitationCorrectness,
		baseline.Reference.CitationCorrectness, baseline.MaximumRegression.CitationCorrectness)
	check("faithfulness", report.Metrics.Faithfulness, baseline.Minimum.Faithfulness,
		baseline.Reference.Faithfulness, baseline.MaximumRegression.Faithfulness)
	check("mrr_at_k", report.Metrics.MRRAtK, baseline.Minimum.MRRAtK,
		baseline.Reference.MRRAtK, baseline.MaximumRegression.MRRAtK)
	check("recall_at_k", report.Metrics.RecallAtK, baseline.Minimum.RecallAtK,
		baseline.Reference.RecallAtK, baseline.MaximumRegression.RecallAtK)
	slices.Sort(violations)
	report.Violations = violations
	report.Passed = len(violations) == 0
	if !report.Passed {
		return report, ErrGateFailed
	}
	return report, nil
}

func retrievalMetrics(relevant, ranked []string, topK int) (float64, float64) {
	relevantSet := stringSet(relevant)
	limit := min(topK, len(ranked))
	found := 0
	reciprocalRank := 0.0
	for index, identifier := range ranked[:limit] {
		if _, ok := relevantSet[identifier]; !ok {
			continue
		}
		found++
		if reciprocalRank == 0 {
			reciprocalRank = 1 / float64(index+1)
		}
	}
	return float64(found) / float64(len(relevantSet)), reciprocalRank
}

func setF1(expected, actual []string) float64 {
	expectedSet, actualSet := stringSet(expected), stringSet(actual)
	truePositive := 0
	for identifier := range actualSet {
		if _, ok := expectedSet[identifier]; ok {
			truePositive++
		}
	}
	if truePositive == 0 {
		return 0
	}
	precision := float64(truePositive) / float64(len(actualSet))
	recall := float64(truePositive) / float64(len(expectedSet))
	return 2 * precision * recall / (precision + recall)
}

func faithfulness(golden GoldenCase, prediction Prediction) float64 {
	answer := strings.ToLower(prediction.Answer)
	citations := stringSet(prediction.CitedArtifactIDs)
	assertions, supported := 0, 0
	for _, claim := range golden.Claims {
		asserted := false
		for _, phrase := range claim.MatchAny {
			if strings.Contains(answer, strings.ToLower(phrase)) {
				asserted = true
				break
			}
		}
		if !asserted {
			continue
		}
		assertions++
		for _, citation := range claim.SupportingArtifactIDs {
			if _, ok := citations[citation]; ok {
				supported++
				break
			}
		}
	}
	for _, contradiction := range golden.Contradictions {
		if strings.Contains(answer, strings.ToLower(contradiction)) {
			assertions++
		}
	}
	if assertions == 0 {
		return 0
	}
	return float64(supported) / float64(assertions)
}
