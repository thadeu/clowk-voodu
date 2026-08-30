package manifest

import (
	"encoding/json"
	"strings"
	"testing"

	"go.voodu.clowk.in/internal/controller"
)

// specOf decodes a parsed deployment manifest into its spec.
func specOf(t *testing.T, m controller.Manifest) DeploymentSpec {
	t.Helper()

	var spec DeploymentSpec

	if err := json.Unmarshal(m.Spec, &spec); err != nil {
		t.Fatalf("decode spec: %v", err)
	}

	return spec
}

// TestDeploymentCollectsNestedPluginBlock is the parser half of the
// nested-plugin contract: a block the core does not recognise inside a
// deployment is carried on the spec instead of rejected, so the
// controller can hand it to the plugin that owns it.
//
// Rejecting it here would mean every plugin wanting a nested block has
// to land a change in the core parser first.
func TestDeploymentCollectsNestedPluginBlock(t *testing.T) {
	src := `
deployment "prod" "esl" {
  image    = "ghcr.io/clowk/esl:1.2"
  replicas = 3

  trafik {
    bind = "0.0.0.0:8084"
    port = 8084
  }
}
`

	mans, err := ParseFile(writeTemp(t, "dep.hcl", src), nil)

	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	spec := specOf(t, mans[0])

	if spec.Image != "ghcr.io/clowk/esl:1.2" || spec.Replicas != 3 {
		t.Fatalf("known fields lost: %+v", spec)
	}

	if len(spec.PluginBlocks) != 1 {
		t.Fatalf("got %d plugin blocks, want 1", len(spec.PluginBlocks))
	}

	blk := spec.PluginBlocks[0]

	if blk.Type != "trafik" {
		t.Errorf("type = %q, want trafik", blk.Type)
	}

	var attrs struct {
		Bind string `json:"bind"`
		Port int    `json:"port"`
	}

	if err := json.Unmarshal(blk.Spec, &attrs); err != nil {
		t.Fatalf("decode plugin block spec: %v", err)
	}

	if attrs.Bind != "0.0.0.0:8084" || attrs.Port != 8084 {
		t.Errorf("attrs = %+v, want bind 0.0.0.0:8084 port 8084", attrs)
	}
}

// TestDeploymentKeepsNestedPluginBlockOrder matters because two blocks
// of the same type are two distinct resources — two listeners on two
// ports. Collapsing them into a map would silently drop one.
func TestDeploymentKeepsNestedPluginBlockOrder(t *testing.T) {
	src := `
deployment "prod" "esl" {
  image = "x"

  trafik { port = 8084 }
  trafik { port = 9090 }
}
`

	mans, err := ParseFile(writeTemp(t, "dep.hcl", src), nil)

	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	blocks := specOf(t, mans[0]).PluginBlocks

	if len(blocks) != 2 {
		t.Fatalf("got %d plugin blocks, want 2", len(blocks))
	}

	for i, wantPort := range []float64{8084, 9090} {
		var attrs map[string]any

		if err := json.Unmarshal(blocks[i].Spec, &attrs); err != nil {
			t.Fatalf("block %d: %v", i, err)
		}

		if attrs["port"] != wantPort {
			t.Errorf("block %d port = %v, want %v", i, attrs["port"], wantPort)
		}
	}
}

// TestDeploymentKeepsNestedPluginBlockLabels preserves the label
// convention plugin kinds already use at the top level, so a nested
// block can name what it configures.
func TestDeploymentKeepsNestedPluginBlockLabels(t *testing.T) {
	src := `
deployment "prod" "esl" {
  image = "x"

  trafik "public" {
    port = 8084
  }
}
`

	mans, err := ParseFile(writeTemp(t, "dep.hcl", src), nil)

	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	blocks := specOf(t, mans[0]).PluginBlocks

	if len(blocks) != 1 {
		t.Fatalf("got %d plugin blocks, want 1", len(blocks))
	}

	if got := blocks[0].Labels; len(got) != 1 || got[0] != "public" {
		t.Errorf("labels = %v, want [public]", got)
	}
}

// TestDeploymentWithoutPluginBlocksStaysEmpty keeps the field out of
// the wire shape for the overwhelming majority of manifests that have
// no plugin block at all.
func TestDeploymentWithoutPluginBlocksStaysEmpty(t *testing.T) {
	src := `
deployment "prod" "esl" {
  image = "x"

  probes {
    readiness {
      http_get {
        path = "/healthz"
        port = 8080
      }
    }
  }
}
`

	mans, err := ParseFile(writeTemp(t, "dep.hcl", src), nil)

	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if blocks := specOf(t, mans[0]).PluginBlocks; len(blocks) != 0 {
		t.Fatalf("known blocks leaked into PluginBlocks: %+v", blocks)
	}

	if strings.Contains(string(mans[0].Spec), "plugin_blocks") {
		t.Error("empty plugin_blocks should be omitted from the wire shape")
	}
}

// TestStatefulsetCollectsNestedPluginBlock — a statefulset rolls its
// replicas the same way, so it needs the same door.
func TestStatefulsetCollectsNestedPluginBlock(t *testing.T) {
	src := `
statefulset "data" "pg" {
  image    = "postgres:16"
  replicas = 2

  trafik {
    bind = "0.0.0.0:5432"
    port = 5432
  }
}
`

	mans, err := ParseFile(writeTemp(t, "sts.hcl", src), nil)

	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	var spec StatefulsetSpec

	if err := json.Unmarshal(mans[0].Spec, &spec); err != nil {
		t.Fatalf("decode spec: %v", err)
	}

	if len(spec.PluginBlocks) != 1 || spec.PluginBlocks[0].Type != "trafik" {
		t.Fatalf("plugin blocks = %+v, want one trafik block", spec.PluginBlocks)
	}
}
