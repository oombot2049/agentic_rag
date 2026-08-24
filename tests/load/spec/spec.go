// Package spec validates load profiles and produces deterministic capacity reports.
package spec

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"strings"
)

const SchemaVersion = 1

var ErrInvalid = errors.New("invalid load evidence")

type Profile struct {
	SchemaVersion int     `json:"schema_version"`
	ProfileID     string  `json:"profile_id"`
	EvidenceClass string  `json:"evidence_class"`
	Stages        []Stage `json:"stages"`
	Limits        Limits  `json:"limits"`
}

type Stage struct {
	Name            string  `json:"name"`
	DurationSeconds int     `json:"duration_seconds"`
	TurnRPS         float64 `json:"turn_rps"`
	SSEConnections  int     `json:"sse_connections"`
	SSEReconnectRPS float64 `json:"sse_reconnect_rps"`
	Burst           bool    `json:"burst"`
}

type Limits struct {
	MinimumAchievedRatio       float64 `json:"minimum_achieved_ratio"`
	MaximumErrorRatio          float64 `json:"maximum_error_ratio"`
	CreateTurnP95MS            float64 `json:"create_turn_p95_ms"`
	CreateTurnP99MS            float64 `json:"create_turn_p99_ms"`
	SSEResumeP95MS             float64 `json:"sse_resume_p95_ms"`
	SSEResumeP99MS             float64 `json:"sse_resume_p99_ms"`
	TTFTP95MS                  float64 `json:"ttft_p95_ms"`
	TTFTP99MS                  float64 `json:"ttft_p99_ms"`
	MaximumSustainedQueueDepth int     `json:"maximum_sustained_queue_depth"`
	MaximumBurstQueueDepth     int     `json:"maximum_burst_queue_depth"`
	MaximumBurstBacklog        int     `json:"maximum_burst_backlog"`
	MaximumDatabaseCPUPercent  float64 `json:"maximum_database_cpu_percent"`
	MaximumServiceCPUPercent   float64 `json:"maximum_service_cpu_percent"`
	MaximumServiceRSSBytes     uint64  `json:"maximum_service_rss_bytes"`
}

type Measurement struct {
	SchemaVersion  int           `json:"schema_version"`
	ProfileID      string        `json:"profile_id"`
	BuildRevision  string        `json:"build_revision"`
	EnvironmentID  string        `json:"environment_id"`
	Representative bool          `json:"representative"`
	Samples        []StageSample `json:"samples"`
}

type StageSample struct {
	StageName            string  `json:"stage_name"`
	AchievedTurnRPS      float64 `json:"achieved_turn_rps"`
	ErrorRatio           float64 `json:"error_ratio"`
	CreateTurnP50MS      float64 `json:"create_turn_p50_ms"`
	CreateTurnP95MS      float64 `json:"create_turn_p95_ms"`
	CreateTurnP99MS      float64 `json:"create_turn_p99_ms"`
	SSEActiveConnections int     `json:"sse_active_connections"`
	SSEReconnectRPS      float64 `json:"sse_reconnect_rps"`
	SSEResumeP50MS       float64 `json:"sse_resume_p50_ms"`
	SSEResumeP95MS       float64 `json:"sse_resume_p95_ms"`
	SSEResumeP99MS       float64 `json:"sse_resume_p99_ms"`
	TTFTP50MS            float64 `json:"ttft_p50_ms"`
	TTFTP95MS            float64 `json:"ttft_p95_ms"`
	TTFTP99MS            float64 `json:"ttft_p99_ms"`
	QueueDepthStart      int     `json:"queue_depth_start"`
	QueueDepthEnd        int     `json:"queue_depth_end"`
	QueueWaitP95MS       float64 `json:"queue_wait_p95_ms"`
	QueueWaitP99MS       float64 `json:"queue_wait_p99_ms"`
	DatabaseCPUPercent   float64 `json:"database_cpu_percent"`
	DatabaseConnections  int     `json:"database_connections"`
	DatabaseLockWaits    int     `json:"database_lock_waits"`
	ServiceCPUPercent    float64 `json:"service_cpu_percent"`
	ServiceRSSBytes      uint64  `json:"service_rss_bytes"`
	ServiceGoroutines    int     `json:"service_goroutines"`
	RequestBytes         uint64  `json:"request_bytes"`
	ResponseBytes        uint64  `json:"response_bytes"`
}

type Report struct {
	SchemaVersion    int          `json:"schema_version"`
	ProfileID        string       `json:"profile_id"`
	BuildRevision    string       `json:"build_revision"`
	EnvironmentID    string       `json:"environment_id"`
	CapacityEvidence bool         `json:"capacity_evidence"`
	Passed           bool         `json:"passed"`
	DegradationStage *string      `json:"degradation_stage"`
	Curve            []CurvePoint `json:"curve"`
	Violations       []string     `json:"violations"`
}

type CurvePoint struct {
	StageName           string  `json:"stage_name"`
	TargetTurnRPS       float64 `json:"target_turn_rps"`
	AchievedTurnRPS     float64 `json:"achieved_turn_rps"`
	SSEConnections      int     `json:"sse_connections"`
	QueueDepthEnd       int     `json:"queue_depth_end"`
	QueueSlopePerSecond float64 `json:"queue_slope_per_second"`
	DatabaseCPUPercent  float64 `json:"database_cpu_percent"`
	ServiceCPUPercent   float64 `json:"service_cpu_percent"`
	ServiceRSSBytes     uint64  `json:"service_rss_bytes"`
}

func LoadProfile(data []byte) (Profile, error) {
	var value Profile
	if err := decode(data, &value); err != nil || validateProfile(value) != nil {
		return Profile{}, ErrInvalid
	}
	return value, nil
}

func LoadMeasurement(data []byte, profile Profile) (Measurement, error) {
	var value Measurement
	if err := decode(data, &value); err != nil || validateMeasurement(value, profile) != nil {
		return Measurement{}, ErrInvalid
	}
	return value, nil
}

func Analyze(profile Profile, measurement Measurement) (Report, error) {
	if validateProfile(profile) != nil || validateMeasurement(measurement, profile) != nil {
		return Report{}, ErrInvalid
	}
	report := Report{SchemaVersion: SchemaVersion, ProfileID: profile.ProfileID,
		BuildRevision: measurement.BuildRevision, EnvironmentID: measurement.EnvironmentID,
		CapacityEvidence: measurement.Representative && profile.EvidenceClass == "RELEASE",
		Passed:           true, Curve: make([]CurvePoint, 0, len(profile.Stages)), Violations: []string{}}
	for index, stage := range profile.Stages {
		sample := measurement.Samples[index]
		point := CurvePoint{StageName: stage.Name, TargetTurnRPS: stage.TurnRPS, AchievedTurnRPS: sample.AchievedTurnRPS,
			SSEConnections: sample.SSEActiveConnections, QueueDepthEnd: sample.QueueDepthEnd,
			QueueSlopePerSecond: float64(sample.QueueDepthEnd-sample.QueueDepthStart) / float64(stage.DurationSeconds),
			DatabaseCPUPercent:  sample.DatabaseCPUPercent, ServiceCPUPercent: sample.ServiceCPUPercent,
			ServiceRSSBytes: sample.ServiceRSSBytes}
		report.Curve = append(report.Curve, point)
		stageViolations := evaluateStage(stage, profile.Limits, sample, point.QueueSlopePerSecond)
		if len(stageViolations) > 0 && report.DegradationStage == nil {
			name := stage.Name
			report.DegradationStage = &name
		}
		report.Violations = append(report.Violations, stageViolations...)
	}
	slices.Sort(report.Violations)
	report.Passed = len(report.Violations) == 0
	report.CapacityEvidence = report.CapacityEvidence && report.Passed
	return report, nil
}

func evaluateStage(stage Stage, limits Limits, sample StageSample, queueSlope float64) []string {
	var result []string
	add := func(condition bool, metric string, observed, limit float64) {
		if condition {
			result = append(result, fmt.Sprintf("%s %s observed=%.3f limit=%.3f", stage.Name, metric, observed, limit))
		}
	}
	add(sample.AchievedTurnRPS+1e-9 < stage.TurnRPS*limits.MinimumAchievedRatio, "achieved_turn_rps", sample.AchievedTurnRPS, stage.TurnRPS*limits.MinimumAchievedRatio)
	add(sample.ErrorRatio > limits.MaximumErrorRatio+1e-12, "error_ratio", sample.ErrorRatio, limits.MaximumErrorRatio)
	add(sample.CreateTurnP95MS > limits.CreateTurnP95MS, "create_turn_p95_ms", sample.CreateTurnP95MS, limits.CreateTurnP95MS)
	add(sample.CreateTurnP99MS > limits.CreateTurnP99MS, "create_turn_p99_ms", sample.CreateTurnP99MS, limits.CreateTurnP99MS)
	if stage.SSEReconnectRPS > 0 {
		add(sample.SSEReconnectRPS+1e-9 < stage.SSEReconnectRPS*limits.MinimumAchievedRatio, "sse_reconnect_rps", sample.SSEReconnectRPS, stage.SSEReconnectRPS*limits.MinimumAchievedRatio)
		add(sample.SSEResumeP95MS > limits.SSEResumeP95MS, "sse_resume_p95_ms", sample.SSEResumeP95MS, limits.SSEResumeP95MS)
		add(sample.SSEResumeP99MS > limits.SSEResumeP99MS, "sse_resume_p99_ms", sample.SSEResumeP99MS, limits.SSEResumeP99MS)
	}
	add(sample.TTFTP95MS > limits.TTFTP95MS, "ttft_p95_ms", sample.TTFTP95MS, limits.TTFTP95MS)
	add(sample.TTFTP99MS > limits.TTFTP99MS, "ttft_p99_ms", sample.TTFTP99MS, limits.TTFTP99MS)
	add(float64(sample.SSEActiveConnections)+1e-9 < float64(stage.SSEConnections)*limits.MinimumAchievedRatio,
		"sse_active_connections", float64(sample.SSEActiveConnections), float64(stage.SSEConnections)*limits.MinimumAchievedRatio)
	if stage.Burst {
		add(sample.QueueDepthEnd > limits.MaximumBurstQueueDepth, "queue_depth", float64(sample.QueueDepthEnd), float64(limits.MaximumBurstQueueDepth))
		add(sample.QueueDepthEnd-sample.QueueDepthStart > limits.MaximumBurstBacklog, "queue_backlog_delta",
			float64(sample.QueueDepthEnd-sample.QueueDepthStart), float64(limits.MaximumBurstBacklog))
	} else {
		add(sample.QueueDepthEnd > limits.MaximumSustainedQueueDepth, "queue_depth", float64(sample.QueueDepthEnd), float64(limits.MaximumSustainedQueueDepth))
		add(queueSlope > 0, "queue_slope_per_second", queueSlope, 0)
	}
	add(sample.DatabaseCPUPercent > limits.MaximumDatabaseCPUPercent, "database_cpu_percent", sample.DatabaseCPUPercent, limits.MaximumDatabaseCPUPercent)
	add(sample.ServiceCPUPercent > limits.MaximumServiceCPUPercent, "service_cpu_percent", sample.ServiceCPUPercent, limits.MaximumServiceCPUPercent)
	add(sample.ServiceRSSBytes > limits.MaximumServiceRSSBytes, "service_rss_bytes", float64(sample.ServiceRSSBytes), float64(limits.MaximumServiceRSSBytes))
	return result
}

func validateProfile(value Profile) error {
	if value.SchemaVersion != SchemaVersion || !validName(value.ProfileID) ||
		(value.EvidenceClass != "SMOKE" && value.EvidenceClass != "RELEASE") || len(value.Stages) == 0 || len(value.Stages) > 20 ||
		!unit(value.Limits.MinimumAchievedRatio) || !unit(value.Limits.MaximumErrorRatio) ||
		value.Limits.CreateTurnP95MS <= 0 || value.Limits.CreateTurnP99MS < value.Limits.CreateTurnP95MS ||
		value.Limits.SSEResumeP95MS <= 0 || value.Limits.SSEResumeP99MS < value.Limits.SSEResumeP95MS ||
		value.Limits.TTFTP95MS <= 0 || value.Limits.TTFTP99MS < value.Limits.TTFTP95MS ||
		value.Limits.MaximumSustainedQueueDepth < 1 || value.Limits.MaximumBurstQueueDepth < value.Limits.MaximumSustainedQueueDepth ||
		value.Limits.MaximumBurstBacklog < 1 || value.Limits.MaximumDatabaseCPUPercent <= 0 || value.Limits.MaximumServiceCPUPercent <= 0 || value.Limits.MaximumServiceRSSBytes == 0 {
		return ErrInvalid
	}
	seen := map[string]struct{}{}
	for _, stage := range value.Stages {
		if !validName(stage.Name) || stage.DurationSeconds < 1 || stage.DurationSeconds > 24*60*60 ||
			!finitePositive(stage.TurnRPS) || stage.SSEConnections < 0 || stage.SSEConnections > 100_000 ||
			stage.SSEReconnectRPS < 0 || math.IsNaN(stage.SSEReconnectRPS) || math.IsInf(stage.SSEReconnectRPS, 0) {
			return ErrInvalid
		}
		if _, duplicate := seen[stage.Name]; duplicate {
			return ErrInvalid
		}
		seen[stage.Name] = struct{}{}
	}
	return nil
}

func validateMeasurement(value Measurement, profile Profile) error {
	if value.SchemaVersion != SchemaVersion || value.ProfileID != profile.ProfileID || !validName(value.BuildRevision) ||
		!validName(value.EnvironmentID) || len(value.Samples) != len(profile.Stages) {
		return ErrInvalid
	}
	for index, sample := range value.Samples {
		if sample.StageName != profile.Stages[index].Name || !finiteNonNegative(sample.AchievedTurnRPS) || !unit(sample.ErrorRatio) ||
			!sample.valid() {
			return ErrInvalid
		}
	}
	return nil
}

func (s StageSample) valid() bool {
	values := []float64{s.CreateTurnP50MS, s.CreateTurnP95MS, s.CreateTurnP99MS, s.SSEReconnectRPS,
		s.SSEResumeP50MS, s.SSEResumeP95MS, s.SSEResumeP99MS, s.TTFTP50MS, s.TTFTP95MS, s.TTFTP99MS,
		s.QueueWaitP95MS, s.QueueWaitP99MS, s.DatabaseCPUPercent, s.ServiceCPUPercent}
	for _, value := range values {
		if !finiteNonNegative(value) {
			return false
		}
	}
	return s.SSEActiveConnections >= 0 && s.QueueDepthStart >= 0 && s.QueueDepthEnd >= 0 &&
		s.DatabaseConnections >= 0 && s.DatabaseLockWaits >= 0 && s.ServiceGoroutines >= 0 &&
		s.CreateTurnP50MS <= s.CreateTurnP95MS && s.CreateTurnP95MS <= s.CreateTurnP99MS &&
		s.SSEResumeP50MS <= s.SSEResumeP95MS && s.SSEResumeP95MS <= s.SSEResumeP99MS &&
		s.TTFTP50MS <= s.TTFTP95MS && s.TTFTP95MS <= s.TTFTP99MS && s.QueueWaitP95MS <= s.QueueWaitP99MS
}

func decode(data []byte, target any) error {
	if len(data) == 0 || len(data) > 16<<20 || !json.Valid(data) {
		return ErrInvalid
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return ErrInvalid
	}
	return nil
}

func validName(value string) bool {
	return value != "" && len(value) <= 128 && strings.TrimSpace(value) == value &&
		strings.IndexFunc(value, func(r rune) bool {
			return !(r == '-' || r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9')
		}) < 0
}
func unit(value float64) bool { return value >= 0 && value <= 1 && !math.IsNaN(value) }
func finitePositive(value float64) bool {
	return value > 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}
func finiteNonNegative(value float64) bool {
	return value >= 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}
