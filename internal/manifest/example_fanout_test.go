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

// TestExampleFswEslParses pins the shape of the per-file-split fsw-esl
// example. `ParseDir` walks the directory so every per-service file
// validates together — the bootstrap apply path operators run on a
// fresh host.
func TestExampleFswEslParses(t *testing.T) {
	vars := map[string]string{
		"SLACK_WEBHOOK_URL": "https://hooks.slack.example/x",
		"PD_ROUTING_KEY":    "pd-fake",
	}

	mans, err := ParseDir("../../examples/fsw-esl", vars)
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

// TestExampleFswEslIndividualFilesParse asserts each per-service file
// is independently valid HCL — that's the whole point of the split:
// `vd apply -f api.hcl` must work without seeing the others.
func TestExampleFswEslIndividualFilesParse(t *testing.T) {
	vars := map[string]string{
		"SLACK_WEBHOOK_URL": "https://hooks.slack.example/x",
		"PD_ROUTING_KEY":    "pd-fake",
	}

	files := []struct {
		path    string
		wantKind string
		wantName string
	}{
		{"../../examples/fsw-esl/redis.hcl", "redis", "redis"},
		{"../../examples/fsw-esl/rabbitmq.hcl", "statefulset", "rabbitmq"},
		{"../../examples/fsw-esl/api.hcl", "deployment", "api"},
		{"../../examples/fsw-esl/adapter.hcl", "deployment", "adapter"},
		{"../../examples/fsw-esl/controller.hcl", "deployment", "controller"},
		{"../../examples/fsw-esl/events.hcl", "deployment", "events"},
		{"../../examples/fsw-esl/jobs.hcl", "deployment", "jobs"},
	}

	for _, f := range files {
		mans, err := ParseFile(f.path, vars)
		if err != nil {
			t.Errorf("parse %s: %v", f.path, err)
			continue
		}

		if len(mans) != 1 {
			t.Errorf("%s: got %d manifests, want 1", f.path, len(mans))
			continue
		}

		if string(mans[0].Kind) != f.wantKind {
			t.Errorf("%s: kind got %q, want %q", f.path, mans[0].Kind, f.wantKind)
		}

		if mans[0].Name != f.wantName {
			t.Errorf("%s: name got %q, want %q", f.path, mans[0].Name, f.wantName)
		}
	}
}
