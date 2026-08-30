package manifest

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestDrainBlockParses pins the two knobs a workload has over how it
// winds down.
func TestDrainBlockParses(t *testing.T) {
	src := `
deployment "prod" "esl" {
  image = "esl:1"

  drain {
    timeout = "30m"
    grace   = "60s"
  }
}
`

	mans, err := ParseFile(writeTemp(t, "dep.hcl", src), nil)

	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	var spec DeploymentSpec

	if err := json.Unmarshal(mans[0].Spec, &spec); err != nil {
		t.Fatal(err)
	}

	if spec.Drain == nil {
		t.Fatal("drain block was dropped")
	}

	if spec.Drain.Timeout != "30m" || spec.Drain.Grace != "60s" {
		t.Errorf("drain = %+v, want timeout 30m grace 60s", spec.Drain)
	}
}

// TestDrainGraceAloneIsValid — grace is useful with no load balancer
// anywhere near: it is how long the process gets between SIGTERM and
// SIGKILL, which is the whole fix for a worker that loses an in-flight
// write on deploy.
func TestDrainGraceAloneIsValid(t *testing.T) {
	src := `
deployment "prod" "worker" {
  image = "worker:1"

  drain {
    grace = "45s"
  }
}
`

	mans, err := ParseFile(writeTemp(t, "dep.hcl", src), nil)

	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	var spec DeploymentSpec

	if err := json.Unmarshal(mans[0].Spec, &spec); err != nil {
		t.Fatal(err)
	}

	if spec.Drain == nil || spec.Drain.Grace != "45s" {
		t.Fatalf("drain = %+v, want grace 45s", spec.Drain)
	}
}

// TestDrainRejectsUnparseableDuration breaks the platform's usual
// leniency on purpose. Elsewhere a bad duration falls back to a
// default, which is harmless for a probe interval. Here it would cut
// the connections the operator wrote the block to protect, and they
// would only find out in production.
func TestDrainRejectsUnparseableDuration(t *testing.T) {
	for _, tc := range []struct{ field, src string }{
		{"timeout", `drain { timeout = "30min" }`},
		{"grace", `drain { grace = "1 minute" }`},
	} {
		src := "deployment \"prod\" \"esl\" {\n  image = \"x\"\n  " + tc.src + "\n}\n"

		_, err := ParseFile(writeTemp(t, "dep.hcl", src), nil)

		if err == nil {
			t.Errorf("%s: an unparseable duration must fail the parse", tc.field)

			continue
		}

		if !strings.Contains(err.Error(), tc.field) {
			t.Errorf("%s: error should name the field, got: %v", tc.field, err)
		}
	}
}

// TestStatefulsetDrainBlockParses — a statefulset winds down the same
// way.
func TestStatefulsetDrainBlockParses(t *testing.T) {
	src := `
statefulset "data" "pg" {
  image = "postgres:16"

  drain {
    grace = "120s"
  }
}
`

	mans, err := ParseFile(writeTemp(t, "sts.hcl", src), nil)

	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	var spec StatefulsetSpec

	if err := json.Unmarshal(mans[0].Spec, &spec); err != nil {
		t.Fatal(err)
	}

	if spec.Drain == nil || spec.Drain.Grace != "120s" {
		t.Fatalf("drain = %+v, want grace 120s", spec.Drain)
	}
}
