package monitoring_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type ruleFile struct {
	Groups []struct {
		Name  string `yaml:"name"`
		Rules []struct {
			Alert       string            `yaml:"alert"`
			Expr        string            `yaml:"expr"`
			For         string            `yaml:"for"`
			Labels      map[string]string `yaml:"labels"`
			Annotations map[string]string `yaml:"annotations"`
		} `yaml:"rules"`
	} `yaml:"groups"`
}

func TestAlertRulesAreActionableAndBounded(t *testing.T) {
	var rules ruleFile
	decodeYAML(t, "prometheus/agenticrag-alerts.yaml", &rules)
	runbookBytes, err := os.ReadFile(filepath.FromSlash("runbooks/agenticrag-alerts.md"))
	if err != nil {
		t.Fatal(err)
	}
	runbook := strings.ToLower(string(runbookBytes))
	seen := make(map[string]bool)
	for _, group := range rules.Groups {
		if !strings.HasPrefix(group.Name, "agenticrag.") || len(group.Rules) == 0 {
			t.Errorf("invalid group %q", group.Name)
		}
		for _, rule := range group.Rules {
			if rule.Alert == "" || rule.Expr == "" || seen[rule.Alert] {
				t.Fatalf("invalid or duplicate alert %q", rule.Alert)
			}
			seen[rule.Alert] = true
			serialized := strings.ToLower(rule.Expr + strings.Join(mapValues(rule.Labels), " ") + strings.Join(mapValues(rule.Annotations), " "))
			for _, forbidden := range []string{"tenant_id", "session_id", "run_id", "request_id", "object_key"} {
				if strings.Contains(serialized, forbidden) {
					t.Errorf("%s contains unbounded/sensitive dimension %s", rule.Alert, forbidden)
				}
			}
			for _, key := range []string{"service", "component", "severity", "priority", "owner"} {
				if rule.Labels[key] == "" {
					t.Errorf("%s missing label %s", rule.Alert, key)
				}
			}
			for _, key := range []string{"summary", "impact", "action", "dashboard", "runbook_url"} {
				if rule.Annotations[key] == "" {
					t.Errorf("%s missing annotation %s", rule.Alert, key)
				}
			}
			if rule.Labels["priority"] == "p0" && (rule.Labels["severity"] != "page" || rule.For == "") {
				t.Errorf("P0 %s must page after a non-empty hold period", rule.Alert)
			}
			anchor := "#" + strings.ToLower(rule.Alert)
			if !strings.HasSuffix(rule.Annotations["runbook_url"], anchor) || !strings.Contains(runbook, "## "+strings.ToLower(rule.Alert)) {
				t.Errorf("%s has no matching runbook anchor", rule.Alert)
			}
		}
	}
	for _, required := range []string{
		"AgenticRAGAvailabilityFastBurn", "AgenticRAGAvailabilitySustainedBurn", "AgenticRAGHTTPP99LatencyHigh",
		"AgenticRAGRunP99LatencyHigh", "AgenticRAGQueueBacklogHigh", "AgenticRAGQueueWaitP95High",
		"AgenticRAGWorkerSaturation", "AgenticRAGLLMHourlyCostHigh", "AgenticRAGPostgresUnavailable",
		"AgenticRAGRedisUnavailable", "AgenticRAGObjectStorageUnavailable", "AgenticRAGSearchIndexUnavailable",
		"AgenticRAGLLMProviderFailureHigh",
	} {
		if !seen[required] {
			t.Errorf("missing required alert %s", required)
		}
	}
}

func TestBurnAlertsUseBothWindowsAndPolicyThresholds(t *testing.T) {
	var rules ruleFile
	decodeYAML(t, "prometheus/agenticrag-alerts.yaml", &rules)
	expressions := make(map[string]string)
	for _, group := range rules.Groups {
		for _, rule := range group.Rules {
			expressions[rule.Alert] = strings.ReplaceAll(rule.Expr, " ", "")
		}
	}
	for name, tokens := range map[string][]string{
		"AgenticRAGAvailabilityFastBurn":      {"[5m]", "[1h]", ">0.0144", "and"},
		"AgenticRAGAvailabilitySustainedBurn": {"[30m]", "[6h]", ">0.006", "and"},
	} {
		for _, token := range tokens {
			if !strings.Contains(expressions[name], token) {
				t.Errorf("%s expression missing %s", name, token)
			}
		}
	}
}

func TestMultiWindowPolicyRejectsSingleWindowNoise(t *testing.T) {
	pages := func(shortBad, shortTotal, longBad, longTotal, multiplier float64) bool {
		return shortTotal > 0 && longTotal > 0 && shortBad/shortTotal > multiplier*0.001 &&
			longBad/longTotal > multiplier*0.001
	}
	if pages(15, 1000, 1, 1000, 14.4) {
		t.Fatal("short spike must not page when the long window is healthy")
	}
	if !pages(15, 1000, 15, 1000, 14.4) {
		t.Fatal("paired fast-burn breach must page")
	}
	if pages(6, 1000, 6, 1000, 6) {
		t.Fatal("threshold equality must not page before it is exceeded")
	}
}

func TestAlertmanagerAndQueueCollectorContracts(t *testing.T) {
	var manager struct {
		Route struct {
			GroupBy []string `yaml:"group_by"`
			Routes  []struct {
				Receiver string   `yaml:"receiver"`
				Matchers []string `yaml:"matchers"`
			} `yaml:"routes"`
		} `yaml:"route"`
		Receivers []struct {
			Name     string `yaml:"name"`
			Webhooks []struct {
				URLFile string `yaml:"url_file"`
			} `yaml:"webhook_configs"`
		} `yaml:"receivers"`
	}
	decodeYAML(t, "alertmanager/alertmanager.yaml", &manager)
	if len(manager.Route.Routes) != 2 || len(manager.Receivers) != 2 {
		t.Fatalf("alertmanager routes=%d receivers=%d", len(manager.Route.Routes), len(manager.Receivers))
	}
	for _, receiver := range manager.Receivers {
		if len(receiver.Webhooks) != 1 || !strings.HasPrefix(receiver.Webhooks[0].URLFile, "/etc/alertmanager/secrets/") {
			t.Errorf("receiver %s must use a mounted URL secret", receiver.Name)
		}
	}
	collector, err := os.ReadFile(filepath.FromSlash("exporters/postgres-queries.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(collector)
	for _, required := range []string{"agenticrag_run_queue_messages", "GROUP BY status", "key_labels: [status]", "type: gauge"} {
		if !strings.Contains(text, required) {
			t.Errorf("queue collector missing %q", required)
		}
	}
	if strings.Contains(strings.ToLower(text), "tenant_id") {
		t.Error("queue metric must not expose tenant_id")
	}
}

func decodeYAML(t *testing.T, path string, destination any) {
	t.Helper()
	content, err := os.ReadFile(filepath.FromSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(content, destination); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
}

func mapValues(values map[string]string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		result = append(result, value)
	}
	return result
}
