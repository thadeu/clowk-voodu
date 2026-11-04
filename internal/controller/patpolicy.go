// patpolicy.go holds what the PAT plane accepts in an apply.
//
// The localhost plane trusts its caller: it is the operator on the box. The
// PAT plane is reachable from the network, and its main client is a hosted
// control plane (Anway Cloud) that creates instances for customers. A stolen
// token there must not become root on the host, so the PAT apply takes an
// ALLOWLIST of kinds and spec keys, not a denylist: a field added to the
// manifest later is refused here until someone decides it is safe.
//
// What the allowlist keeps out, and why:
//
//   - volumes, env_file, network_mode, networks, cap_add, ulimits,
//     docker_options, extra_hosts: each one reaches the host (its files,
//     its network stack, its kernel) or passes raw docker flags.
//   - build: a build runs a Dockerfile on the host. The PAT plane runs
//     images only.
//   - on_deploy, on_probe, post_deploy, release: hooks and webhooks that
//     make the controller call URLs or run release commands.
//   - plugin kinds (postgres, redis, ...): an unknown kind installs a
//     plugin, and a plugin install runs its hooks as the controller user.
//   - public ports: `ports = ["7419"]` publishes on a random public host
//     port. Only loopback binds ("127.0.0.1:H:C") pass; ingress reaches
//     the container on the voodu0 network without a published port.
//   - on-demand TLS on an ingress: its ask URL makes Caddy call out.

package controller

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// patWorkloadKeys are the deployment and statefulset spec keys the PAT
// plane accepts.
var patWorkloadKeys = map[string]bool{
	"image":         true,
	"replicas":      true,
	"command":       true,
	"env":           true,
	"ports":         true,
	"restart":       true,
	"health_check":  true,
	"volume_claims": true,
	"depends_on":    true,
	"resources":     true,
	"logs":          true,
	"inits":         true,
	"probes":        true,
	"drain":         true,
	"autoscale":     true,
	"keep_releases": true,
}

// patInitKeys are the init container keys the PAT plane accepts.
var patInitKeys = map[string]bool{
	"name":      true,
	"image":     true,
	"command":   true,
	"timeout":   true,
	"retries":   true,
	"resources": true,
}

// patIngressKeys are the ingress spec keys the PAT plane accepts.
var patIngressKeys = map[string]bool{
	"host":      true,
	"service":   true,
	"port":      true,
	"tls":       true,
	"locations": true,
	"lb":        true,
	"tcp":       true,
}

// validatePATManifests refuses a batch that the PAT plane must not apply,
// or that reaches a resource scope the PAT does not hold. It runs before
// the shared apply path, on the manifests as the caller sent them.
func validatePATManifests(pat *PAT, manifests []*Manifest) error {
	for _, m := range manifests {
		if m == nil {
			return fmt.Errorf("null manifest in body")
		}

		ref := fmt.Sprintf("%s/%s/%s", m.Kind, m.Scope, m.Name)

		if pat != nil && !pat.AllowsResourceScope(m.Scope) {
			return fmt.Errorf("%s: this token cannot use scope %q", ref, m.Scope)
		}

		spec, err := specKeys(m.Spec)
		if err != nil {
			return fmt.Errorf("%s: %w", ref, err)
		}

		switch m.Kind {
		case KindDeployment, KindStatefulset:
			err = validatePATWorkload(spec)
		case KindIngress:
			err = validatePATIngress(spec)
		default:
			err = fmt.Errorf("kind %q is not accepted on the PAT plane (deployment, statefulset, ingress)", m.Kind)
		}

		if err != nil {
			return fmt.Errorf("%s: %w", ref, err)
		}
	}

	return nil
}

func specKeys(raw json.RawMessage) (map[string]json.RawMessage, error) {
	spec := map[string]json.RawMessage{}

	if len(raw) == 0 {
		return spec, nil
	}

	if err := json.Unmarshal(raw, &spec); err != nil {
		return nil, fmt.Errorf("decode spec: %w", err)
	}

	return spec, nil
}

func refuseUnknownKeys(spec map[string]json.RawMessage, allowed map[string]bool, where string) error {
	var refused []string

	for k := range spec {
		if !allowed[k] {
			refused = append(refused, k)
		}
	}

	if len(refused) == 0 {
		return nil
	}

	sort.Strings(refused)

	return fmt.Errorf("%s: %s not accepted on the PAT plane", where, strings.Join(refused, ", "))
}

func validatePATWorkload(spec map[string]json.RawMessage) error {
	if err := refuseUnknownKeys(spec, patWorkloadKeys, "spec"); err != nil {
		return err
	}

	var image string

	if raw, ok := spec["image"]; ok {
		_ = json.Unmarshal(raw, &image)
	}

	if strings.TrimSpace(image) == "" {
		return fmt.Errorf("image is required: the PAT plane does not build")
	}

	if raw, ok := spec["ports"]; ok {
		var ports []string

		if err := json.Unmarshal(raw, &ports); err != nil {
			return fmt.Errorf("decode ports: %w", err)
		}

		for _, p := range ports {
			if !loopbackPort(p) {
				return fmt.Errorf("port %q is public: the PAT plane publishes only 127.0.0.1:<host>:<container>", p)
			}
		}
	}

	if raw, ok := spec["inits"]; ok {
		var inits []map[string]json.RawMessage

		if err := json.Unmarshal(raw, &inits); err != nil {
			return fmt.Errorf("decode inits: %w", err)
		}

		for _, in := range inits {
			if err := refuseUnknownKeys(in, patInitKeys, "inits"); err != nil {
				return err
			}
		}
	}

	return nil
}

// loopbackPort reports whether a docker -p value binds to the host
// loopback only: "127.0.0.1:H:C", with an optional "/tcp" or "/udp".
func loopbackPort(p string) bool {
	p, _, _ = strings.Cut(strings.TrimSpace(p), "/")

	parts := strings.Split(p, ":")

	return len(parts) == 3 && parts[0] == "127.0.0.1" && parts[1] != "" && parts[2] != ""
}

func validatePATIngress(spec map[string]json.RawMessage) error {
	if err := refuseUnknownKeys(spec, patIngressKeys, "spec"); err != nil {
		return err
	}

	raw, ok := spec["tls"]
	if !ok {
		return nil
	}

	var tls map[string]any

	if err := json.Unmarshal(raw, &tls); err != nil {
		return fmt.Errorf("decode tls: %w", err)
	}

	if on, _ := tls["on_demand"].(bool); on {
		return fmt.Errorf("tls.on_demand is not accepted on the PAT plane")
	}

	if ask, _ := tls["ask"].(string); ask != "" {
		return fmt.Errorf("tls.ask is not accepted on the PAT plane")
	}

	return nil
}
