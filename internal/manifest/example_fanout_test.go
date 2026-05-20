package manifest

import (
	"encoding/json"
	"testing"
)

// TestExampleFanoutMultiTargetParses ensures the shipped
// examples/on_deploy/fanout-multi-target.hcl validates against
// the current slice-based on_deploy schema. Catches regressions
// where the example drifts out of sync with the parser surface.
func TestExampleFanoutMultiTargetParses(t *testing.T) {
	vars := map[string]string{
		"SLACK_WEBHOOK_URL": "https://hooks.slack.example/x",
		"DD_API_KEY":        "dd-fake",
		"PD_ROUTING_KEY":    "pd-fake",
		"OPSGENIE_KEY":      "og-fake",
	}

	mans, err := ParseFile("../../examples/on_deploy/fanout-multi-target.hcl", vars)
	if err != nil {
		t.Fatalf("parse fanout example: %v", err)
	}

	deplIdx := -1
	for i := range mans {
		if mans[i].Kind == "deployment" && mans[i].Name == "api" {
			deplIdx = i
			break
		}
	}
	if deplIdx == -1 {
		t.Fatal("deployment/prod/api missing from parsed example")
	}

	var spec DeploymentSpec
	if err := json.Unmarshal(mans[deplIdx].Spec, &spec); err != nil {
		t.Fatalf("decode spec: %v", err)
	}

	if spec.OnDeploy == nil {
		t.Fatal("on_deploy missing")
	}
	if got := len(spec.OnDeploy.Success); got != 3 {
		t.Errorf("success targets: got %d, want 3 (Slack + Datadog + internal status)", got)
	}
	if got := len(spec.OnDeploy.Failure); got != 2 {
		t.Errorf("failure targets: got %d, want 2 (PagerDuty + OpsGenie)", got)
	}
}

// TestExampleFswEslParses ensures the shipped examples/fsw-esl/voodu.hcl
// validates against the current parser. Catches drift between the
// real-world telephony example and the HCL grammar.
func TestExampleFswEslParses(t *testing.T) {
	vars := map[string]string{
		"SLACK_WEBHOOK_URL": "https://hooks.slack.example/x",
		"PD_ROUTING_KEY":    "pd-fake",
	}

	mans, err := ParseFile("../../examples/fsw-esl/voodu.hcl", vars)
	if err != nil {
		t.Fatalf("parse fsw-esl example: %v", err)
	}

	// Count by kind to pin the shape: 1 redis (macro), 1 rabbitmq
	// statefulset, 5 deployments (api/adapter/controller/events/jobs).
	counts := map[string]int{}
	for _, m := range mans {
		counts[string(m.Kind)]++
	}

	if got := counts["redis"]; got != 1 {
		t.Errorf("redis: got %d, want 1", got)
	}
	if got := counts["statefulset"]; got != 1 {
		t.Errorf("statefulset: got %d, want 1 (rabbitmq)", got)
	}
	if got := counts["deployment"]; got != 5 {
		t.Errorf("deployment: got %d, want 5 (api, adapter, controller, events, jobs)", got)
	}
}
