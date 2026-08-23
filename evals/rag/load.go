package rag

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"unicode"
)

var (
	ErrInvalidData = errors.New("invalid RAG evaluation data")
	stableID       = regexp.MustCompile(`^[a-z][a-z0-9_-]{3,127}$`)
	chunkID        = regexp.MustCompile(`^chk_[A-Za-z0-9_-]{4,124}$`)
	artifactID     = regexp.MustCompile(`^art_[A-Za-z0-9_-]{4,124}$`)
	digestPattern  = regexp.MustCompile(`^[a-f0-9]{64}$`)
)

func LoadDataset(data []byte) (Dataset, error) {
	var value Dataset
	if err := decodeStrict(data, &value); err != nil {
		return Dataset{}, fmt.Errorf("%w: dataset: %v", ErrInvalidData, err)
	}
	value.digest = sha256.Sum256(data)
	if err := validateDataset(value); err != nil {
		return Dataset{}, err
	}
	return cloneDataset(value), nil
}

func LoadPredictions(data []byte, dataset Dataset) (PredictionSet, error) {
	var value PredictionSet
	if err := decodeStrict(data, &value); err != nil {
		return PredictionSet{}, fmt.Errorf("%w: predictions: %v", ErrInvalidData, err)
	}
	if err := validatePredictions(value, dataset); err != nil {
		return PredictionSet{}, err
	}
	for index := range value.Predictions {
		item := &value.Predictions[index]
		item.RankedChunkIDs = append([]string(nil), item.RankedChunkIDs...)
		item.CitedArtifactIDs = append([]string(nil), item.CitedArtifactIDs...)
	}
	return value, nil
}

func validatePredictions(value PredictionSet, dataset Dataset) error {
	if value.SchemaVersion != SchemaVersion || value.DatasetID != dataset.DatasetID || len(value.Predictions) != len(dataset.Cases) {
		return fmt.Errorf("%w: prediction header or cardinality", ErrInvalidData)
	}
	expected := make(map[string]struct{}, len(dataset.Cases))
	for _, item := range dataset.Cases {
		expected[item.ID] = struct{}{}
	}
	seen := make(map[string]struct{}, len(value.Predictions))
	for index := range value.Predictions {
		item := &value.Predictions[index]
		if _, ok := expected[item.CaseID]; !ok {
			return fmt.Errorf("%w: unknown prediction case %q", ErrInvalidData, item.CaseID)
		}
		if _, duplicate := seen[item.CaseID]; duplicate {
			return fmt.Errorf("%w: duplicate prediction case %q", ErrInvalidData, item.CaseID)
		}
		seen[item.CaseID] = struct{}{}
		if !validText(item.Answer, 1<<20) || len(item.RankedChunkIDs) > 1000 || len(item.CitedArtifactIDs) > 256 ||
			validateUniqueIDs(item.RankedChunkIDs, chunkID) != nil || validateUniqueIDs(item.CitedArtifactIDs, artifactID) != nil {
			return fmt.Errorf("%w: prediction %q payload", ErrInvalidData, item.CaseID)
		}
	}
	return nil
}

func LoadBaseline(data []byte) (Baseline, error) {
	var value Baseline
	if err := decodeStrict(data, &value); err != nil {
		return Baseline{}, fmt.Errorf("%w: baseline: %v", ErrInvalidData, err)
	}
	if err := validateBaseline(value); err != nil {
		return Baseline{}, err
	}
	return value, nil
}

func validateDataset(value Dataset) error {
	if value.SchemaVersion != SchemaVersion || !stableID.MatchString(value.DatasetID) || !stableID.MatchString(value.Revision) ||
		value.TopK < 1 || value.TopK > 100 || len(value.Cases) < 5 || len(value.Cases) > 100_000 {
		return fmt.Errorf("%w: dataset header", ErrInvalidData)
	}
	seenCases := make(map[string]struct{}, len(value.Cases))
	humanReviewed := 0
	for _, item := range value.Cases {
		if !stableID.MatchString(item.ID) || !validText(item.Query, 8192) || !validText(item.Provenance, 1024) ||
			len(item.RelevantChunkIDs) == 0 || len(item.ExpectedCitationIDs) == 0 || len(item.Claims) == 0 ||
			validateUniqueIDs(item.RelevantChunkIDs, chunkID) != nil || validateUniqueIDs(item.ExpectedCitationIDs, artifactID) != nil {
			return fmt.Errorf("%w: Golden case %q", ErrInvalidData, item.ID)
		}
		if _, duplicate := seenCases[item.ID]; duplicate {
			return fmt.Errorf("%w: duplicate Golden case %q", ErrInvalidData, item.ID)
		}
		seenCases[item.ID] = struct{}{}
		expectedCitations := stringSet(item.ExpectedCitationIDs)
		seenClaims := make(map[string]struct{}, len(item.Claims))
		for _, claim := range item.Claims {
			if !stableID.MatchString(claim.ID) || len(claim.MatchAny) == 0 || len(claim.SupportingArtifactIDs) == 0 ||
				validateUniqueText(claim.MatchAny) != nil || validateUniqueIDs(claim.SupportingArtifactIDs, artifactID) != nil {
				return fmt.Errorf("%w: Golden claim %q", ErrInvalidData, claim.ID)
			}
			if _, duplicate := seenClaims[claim.ID]; duplicate {
				return fmt.Errorf("%w: duplicate Golden claim %q", ErrInvalidData, claim.ID)
			}
			seenClaims[claim.ID] = struct{}{}
			for _, citation := range claim.SupportingArtifactIDs {
				if _, ok := expectedCitations[citation]; !ok {
					return fmt.Errorf("%w: claim support %q is not an expected citation", ErrInvalidData, citation)
				}
			}
		}
		if validateUniqueText(item.Contradictions) != nil {
			return fmt.Errorf("%w: Golden contradictions %q", ErrInvalidData, item.ID)
		}
		if item.HumanReviewed {
			humanReviewed++
			if !validText(item.ReviewNote, 2048) {
				return fmt.Errorf("%w: reviewed case %q requires a note", ErrInvalidData, item.ID)
			}
		} else if item.ReviewNote != "" {
			return fmt.Errorf("%w: unreviewed case %q has a review note", ErrInvalidData, item.ID)
		}
	}
	if humanReviewed*5 < len(value.Cases) {
		return fmt.Errorf("%w: fewer than 20%% of cases are human reviewed", ErrInvalidData)
	}
	return nil
}

func decodeStrict(data []byte, target any) error {
	if len(data) == 0 || len(data) > 64<<20 || !json.Valid(data) || rejectDuplicateKeys(data) != nil {
		return errors.New("JSON is empty, oversized, or invalid")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON value")
	}
	return nil
}

func rejectDuplicateKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	var walk func() error
	walk = func() error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delimiter, compound := token.(json.Delim)
		if !compound {
			return nil
		}
		switch delimiter {
		case '{':
			seen := map[string]struct{}{}
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
					return fmt.Errorf("duplicate JSON key %q", key)
				}
				seen[key] = struct{}{}
				if err := walk(); err != nil {
					return err
				}
			}
		case '[':
			for decoder.More() {
				if err := walk(); err != nil {
					return err
				}
			}
		default:
			return errors.New("unexpected JSON delimiter")
		}
		_, err = decoder.Token()
		return err
	}
	if err := walk(); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON token")
	}
	return nil
}

func validateUniqueIDs(values []string, pattern *regexp.Regexp) error {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if !pattern.MatchString(value) {
			return ErrInvalidData
		}
		if _, duplicate := seen[value]; duplicate {
			return ErrInvalidData
		}
		seen[value] = struct{}{}
	}
	return nil
}

func validateUniqueText(values []string) error {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if !validText(value, 8192) {
			return ErrInvalidData
		}
		canonical := strings.ToLower(value)
		if _, duplicate := seen[canonical]; duplicate {
			return ErrInvalidData
		}
		seen[canonical] = struct{}{}
	}
	return nil
}

func validText(value string, maximum int) bool {
	return value != "" && len(value) <= maximum && strings.TrimSpace(value) == value &&
		strings.IndexFunc(value, unicode.IsControl) < 0
}

func validMetrics(value Metrics) bool {
	return unit(value.RecallAtK) && unit(value.MRRAtK) && unit(value.CitationCorrectness) && unit(value.Faithfulness)
}

func validateBaseline(value Baseline) error {
	if value.SchemaVersion != SchemaVersion || !stableID.MatchString(value.DatasetID) || !digestPattern.MatchString(value.DatasetSHA256) ||
		value.TopK < 1 || value.TopK > 100 || !validMetrics(value.Reference) || !validMetrics(value.Minimum) || !validMetrics(value.MaximumRegression) ||
		value.Minimum.RecallAtK > value.Reference.RecallAtK || value.Minimum.MRRAtK > value.Reference.MRRAtK ||
		value.Minimum.CitationCorrectness > value.Reference.CitationCorrectness || value.Minimum.Faithfulness > value.Reference.Faithfulness {
		return fmt.Errorf("%w: baseline fields", ErrInvalidData)
	}
	return nil
}

func unit(value float64) bool { return value >= 0 && value <= 1 }
func stringSet(values []string) map[string]struct{} {
	result := make(map[string]struct{}, len(values))
	for _, value := range values {
		result[value] = struct{}{}
	}
	return result
}

func cloneDataset(value Dataset) Dataset {
	result := value
	result.Cases = append([]GoldenCase(nil), value.Cases...)
	for index := range result.Cases {
		item := &result.Cases[index]
		item.RelevantChunkIDs = append([]string(nil), item.RelevantChunkIDs...)
		item.ExpectedCitationIDs = append([]string(nil), item.ExpectedCitationIDs...)
		item.Contradictions = append([]string(nil), item.Contradictions...)
		item.Claims = append([]GoldenClaim(nil), item.Claims...)
		for claimIndex := range item.Claims {
			item.Claims[claimIndex].MatchAny = append([]string(nil), item.Claims[claimIndex].MatchAny...)
			item.Claims[claimIndex].SupportingArtifactIDs = append([]string(nil), item.Claims[claimIndex].SupportingArtifactIDs...)
		}
	}
	return result
}

func parseDigest(value string) ([sha256.Size]byte, error) {
	var result [sha256.Size]byte
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != sha256.Size {
		return result, ErrInvalidData
	}
	copy(result[:], decoded)
	return result, nil
}
