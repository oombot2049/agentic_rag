package spec_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agenticrag/agenticrag/tests/load/spec"
)

func TestContractFixturePassesButIsNotCapacityEvidence(t *testing.T) {
	profile := loadProfile(t, "smoke-v1.json")
	data, err := os.ReadFile(filepath.Join("..", "testdata", "contract-pass.json"))
	if err != nil {
		t.Fatal(err)
	}
	measurement, err := spec.LoadMeasurement(data, profile)
	if err != nil {
		t.Fatal(err)
	}
	report, err := spec.Analyze(profile, measurement)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Passed || report.CapacityEvidence || report.DegradationStage != nil {
		t.Fatalf("unexpected report: %+v", report)
	}
	if len(report.Curve) != 4 || report.Curve[1].StageName != "sustained" || report.Curve[1].QueueSlopePerSecond >= 0 {
		t.Fatalf("unexpected curve: %+v", report.Curve)
	}
}

func TestAnalyzerIdentifiesFirstDegradationStage(t *testing.T) {
	profile := loadProfile(t, "smoke-v1.json")
	data, _ := os.ReadFile(filepath.Join("..", "testdata", "contract-pass.json"))
	measurement, _ := spec.LoadMeasurement(data, profile)
	measurement.Samples[1].QueueDepthEnd = measurement.Samples[1].QueueDepthStart + 10
	measurement.Samples[2].ErrorRatio = 1
	report, err := spec.Analyze(profile, measurement)
	if err != nil {
		t.Fatal(err)
	}
	if report.Passed || report.DegradationStage == nil || *report.DegradationStage != "sustained" {
		t.Fatalf("unexpected report: %+v", report)
	}
	if !strings.Contains(strings.Join(report.Violations, "\n"), "queue_slope_per_second") {
		t.Fatalf("missing queue violation: %v", report.Violations)
	}
}

func TestBurstBacklogIsBounded(t *testing.T) {
	profile := loadProfile(t, "smoke-v1.json")
	data, _ := os.ReadFile(filepath.Join("..", "testdata", "contract-pass.json"))
	measurement, _ := spec.LoadMeasurement(data, profile)
	measurement.Samples[2].QueueDepthEnd = measurement.Samples[2].QueueDepthStart + 3001
	report, _ := spec.Analyze(profile, measurement)
	if !strings.Contains(strings.Join(report.Violations, "\n"), "queue_backlog_delta") {
		t.Fatalf("missing backlog violation: %v", report.Violations)
	}
}

func TestStrictDecodingRejectsUnknownAndNonFiniteData(t *testing.T) {
	profile := loadProfile(t, "smoke-v1.json")
	bad := []byte(`{"schema_version":1,"profile_id":"smoke-v1","build_revision":"x","environment_id":"ci","representative":false,"samples":[],"extra":true}`)
	if _, err := spec.LoadMeasurement(bad, profile); err == nil {
		t.Fatal("expected unknown-field rejection")
	}
	badProfile := []byte(`{"schema_version":1,"profile_id":"x","evidence_class":"SMOKE","stages":[{"name":"x","duration_seconds":1,"turn_rps":1e999,"sse_connections":0,"sse_reconnect_rps":0,"burst":false}],"limits":{}}`)
	if _, err := spec.LoadProfile(badProfile); err == nil {
		t.Fatal("expected non-finite rejection")
	}
}

func TestReleaseRepresentativeMeasurementCanBeEvidence(t *testing.T) {
	profile := loadProfile(t, "release-v1.json")
	data, _ := os.ReadFile(filepath.Join("..", "testdata", "contract-pass.json"))
	smoke := loadProfile(t, "smoke-v1.json")
	m, _ := spec.LoadMeasurement(data, smoke)
	m.ProfileID = profile.ProfileID
	m.Representative = true
	for i := range m.Samples {
		m.Samples[i].StageName = profile.Stages[i].Name
		m.Samples[i].AchievedTurnRPS = profile.Stages[i].TurnRPS
		m.Samples[i].SSEActiveConnections = profile.Stages[i].SSEConnections
		m.Samples[i].SSEReconnectRPS = profile.Stages[i].SSEReconnectRPS
	}
	r, err := spec.Analyze(profile, m)
	if err != nil {
		t.Fatal(err)
	}
	if !r.CapacityEvidence {
		t.Fatal("expected representative release evidence classification")
	}
	m.Samples[1].ErrorRatio = 1
	r, _ = spec.Analyze(profile, m)
	if r.CapacityEvidence || r.Passed {
		t.Fatal("failed run must not be capacity evidence")
	}
}

func loadProfile(t *testing.T, name string) spec.Profile {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "profiles", name))
	if err != nil {
		t.Fatal(err)
	}
	profile, err := spec.LoadProfile(data)
	if err != nil {
		t.Fatal(err)
	}
	return profile
}
