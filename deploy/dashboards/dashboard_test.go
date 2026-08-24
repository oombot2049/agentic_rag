package dashboards_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type dashboard struct {
	Title, UID, Refresh, Timezone string
	Editable                      bool
	SchemaVersion                 int
	Panels                        []struct {
		ID          int
		Title       string
		Description string
		Datasource  struct{ Type, UID string }
		Targets     []struct {
			Expr, RawSQL string
		}
	}
	Templating struct {
		List []struct {
			Name, Type string
			Query      any
			Datasource struct{ Type, UID string }
			IncludeAll bool
			AllValue   string
			Multi      bool
		}
	}
}

func TestOperationsDashboardCoversTheTechnicalJourneyWithoutIdentityLabels(t *testing.T) {
	value, raw := readDashboard(t, "agenticrag-operations.json")
	validateDashboard(t, value, "agenticrag-operations", "30s")
	titles := panelTitles(value)
	for _, required := range []string{
		"Turn command availability (30d)", "Request rate by status class", "HTTP latency p50 / p95 / p99",
		"Queue backlog", "Queue wait p95 and active Runs", "Retrieval logical availability",
		"LLM outcomes by provider", "LLM TTFT p95", "Run terminal outcomes", "Run duration p95 / p99",
		"Dependency health", "Active P0 pages", "Estimated LLM cost / hour",
	} {
		if !slices.Contains(titles, required) {
			t.Errorf("missing operations panel %q", required)
		}
	}
	for _, forbidden := range []string{"tenant_id", "session_id", "turn_id", "run_id", "request_id", "object_key"} {
		if strings.Contains(strings.ToLower(raw), forbidden) {
			t.Errorf("operations dashboard contains forbidden identity %s", forbidden)
		}
	}
	for _, panel := range value.Panels {
		for _, target := range panel.Targets {
			if target.Expr != "" && !strings.Contains(target.Expr, "agenticrag_") && !strings.Contains(target.Expr, "ALERTS") &&
				!strings.Contains(target.Expr, "probe_success") && !strings.Contains(target.Expr, "up{") {
				t.Errorf("panel %q has unexpected PromQL %q", panel.Title, target.Expr)
			}
		}
	}
}

func TestBusinessDashboardUsesGovernedTenantScopeAndDistinctRunFunnel(t *testing.T) {
	value, _ := readDashboard(t, "agenticrag-business.json")
	validateDashboard(t, value, "agenticrag-business", "5m")
	var tenantFound, funnelFound, reconciliationFound bool
	for _, variable := range value.Templating.List {
		if variable.Name != "tenant" {
			continue
		}
		tenantFound = variable.Type == "query" && variable.IncludeAll && !variable.Multi && variable.AllValue == "__all__" &&
			strings.Contains(stringQuery(variable.Query), "dashboard_tenant_scope")
	}
	for _, panel := range value.Panels {
		if strings.Contains(strings.ToLower(panel.Title), "funnel") {
			funnelFound = true
		}
		if strings.Contains(strings.ToLower(panel.Title), "reconciliation") {
			reconciliationFound = true
		}
		for _, target := range panel.Targets {
			if target.RawSQL == "" {
				continue
			}
			if !strings.Contains(target.RawSQL, "JOIN dashboard_tenant_scope") ||
				!strings.Contains(target.RawSQL, "${tenant:sqlstring}") {
				t.Errorf("panel %q bypasses governed tenant filtering", panel.Title)
			}
			if strings.Contains(target.RawSQL, "analytics_run_funnel_hourly") &&
				(!strings.Contains(strings.ToUpper(target.RawSQL), "COUNT(DISTINCT F.RUN_ID)") ||
					!strings.Contains(target.RawSQL, "GENERATION_STARTED") || !strings.Contains(target.RawSQL, "COMMITTED")) {
				t.Errorf("panel %q violates logical funnel contract", panel.Title)
			}
		}
	}
	if !tenantFound || !funnelFound || !reconciliationFound {
		t.Fatalf("tenant=%v funnel=%v reconciliation=%v", tenantFound, funnelFound, reconciliationFound)
	}
}

func TestLogicalFunnelFixtureCountsEachRunOncePerStage(t *testing.T) {
	type event struct{ run, stage string }
	events := []event{{"run-a", "REQUESTED"}, {"run-a", "QUEUED"}, {"run-a", "RETRIEVED"},
		{"run-a", "RETRIEVED"}, {"run-a", "GENERATION_STARTED"}, {"run-a", "TERMINAL"}, {"run-a", "COMMITTED"},
		{"run-b", "REQUESTED"}, {"run-b", "QUEUED"}, {"run-b", "RETRIEVED"}}
	sets := make(map[string]map[string]struct{})
	for _, item := range events {
		if sets[item.stage] == nil {
			sets[item.stage] = make(map[string]struct{})
		}
		sets[item.stage][item.run] = struct{}{}
	}
	if len(sets["REQUESTED"]) != 2 || len(sets["RETRIEVED"]) != 2 || len(sets["GENERATION_STARTED"]) != 1 || len(sets["COMMITTED"]) != 1 {
		t.Fatalf("unexpected funnel counts: %+v", sets)
	}
	stages := []string{"REQUESTED", "QUEUED", "RETRIEVED", "GENERATION_STARTED", "TERMINAL", "COMMITTED"}
	for index := 1; index < len(stages); index++ {
		if len(sets[stages[index]]) > len(sets[stages[index-1]]) {
			t.Fatalf("funnel increased from %s to %s", stages[index-1], stages[index])
		}
	}
}

func TestProvisioningIsImmutableAndFastToReload(t *testing.T) {
	content, err := os.ReadFile(filepath.FromSlash("provisioning/dashboards.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var value struct {
		APIVersion int `yaml:"apiVersion"`
		Providers  []struct {
			Name                  string
			DisableDeletion       bool `yaml:"disableDeletion"`
			AllowUIUpdates        bool `yaml:"allowUiUpdates"`
			UpdateIntervalSeconds int  `yaml:"updateIntervalSeconds"`
			Options               struct{ Path string }
		}
	}
	if err := yaml.Unmarshal(content, &value); err != nil {
		t.Fatal(err)
	}
	if value.APIVersion != 1 || len(value.Providers) != 1 || !value.Providers[0].DisableDeletion ||
		value.Providers[0].AllowUIUpdates || value.Providers[0].UpdateIntervalSeconds > 60 ||
		value.Providers[0].Options.Path != "/var/lib/grafana/dashboards/agenticrag" {
		t.Fatalf("invalid provisioning: %+v", value)
	}
}

func readDashboard(t *testing.T, name string) (dashboard, string) {
	t.Helper()
	content, err := os.ReadFile(filepath.FromSlash(name))
	if err != nil {
		t.Fatal(err)
	}
	var value dashboard
	if err := json.Unmarshal(content, &value); err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	return value, string(content)
}

func validateDashboard(t *testing.T, value dashboard, uid, refresh string) {
	t.Helper()
	if value.UID != uid || value.Title == "" || value.Editable || value.SchemaVersion < 40 || value.Timezone != "utc" ||
		value.Refresh != refresh || len(value.Panels) < 6 {
		t.Fatalf("invalid dashboard header: %+v", value)
	}
	ids := make(map[int]bool)
	for _, panel := range value.Panels {
		if panel.ID < 1 || panel.Title == "" || ids[panel.ID] || len(panel.Targets) == 0 {
			t.Errorf("invalid panel id=%d title=%q", panel.ID, panel.Title)
		}
		ids[panel.ID] = true
	}
}

func panelTitles(value dashboard) []string {
	result := make([]string, 0, len(value.Panels))
	for _, panel := range value.Panels {
		result = append(result, panel.Title)
	}
	return result
}

func stringQuery(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case map[string]any:
		if query, ok := typed["query"].(string); ok {
			return query
		}
	}
	return ""
}
