package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"

	"go.voodu.clowk.in/pkg/plugin"
)

// ingressSpec mirrors internal/manifest's IngressSpec for the same
// reason as databaseSpec/deploymentSpec (see handlers.go): avoid an
// import cycle and keep the reconciler's view of each kind explicit.
type ingressSpec struct {
	Host      string            `json:"host"`
	Service   string            `json:"service,omitempty"`
	Port      int               `json:"port,omitempty"`
	TLS       *ingressTLS       `json:"tls,omitempty"`
	Locations []ingressLocation `json:"locations,omitempty"`
	LB        *ingressLB        `json:"lb,omitempty"`
}

type ingressLocation struct {
	Path  string `json:"path"`
	Strip bool   `json:"strip,omitempty"`
}

type ingressLB struct {
	Policy   string `json:"policy,omitempty"`
	Interval string `json:"interval,omitempty"`
}

type ingressTLS struct {
	Enabled  bool   `json:"enabled,omitempty"`
	Provider string `json:"provider,omitempty"`
	Email    string `json:"email,omitempty"`

	// OnDemand switches the plugin into on-demand issuance mode. Ask is
	// the callback URL the router uses to gate which hostnames get a
	// cert — typically an internal endpoint on the app itself. Both
	// together unlock wildcard-like behaviour (e.g. *.tenant.example.com)
	// without needing DNS-01 credentials.
	OnDemand bool   `json:"on_demand,omitempty"`
	Ask      string `json:"ask,omitempty"`
}

// IngressHandler dispatches ingress manifests to the ingress plugin
// (voodu-caddy by default). Same shape as DatabaseHandler — the
// reconciler doesn't know how to terminate TLS or route hosts, but
// the plugin does, and the handler's job is to give it the desired
// state plus collect whatever status blob the plugin produces.
type IngressHandler struct {
	Store   Store
	Invoker PluginInvoker
	Log     *log.Logger

	// PluginName lets operators swap the default ingress plugin. Empty
	// defaults to "caddy", matching the milestone's reference plugin.
	PluginName string

	// Containers lets the upstream resolver list live deployment
	// replicas by label. Optional — when nil, deployment upstreams
	// fall back to the bare AppID (single-replica routing). Required
	// for replicas>1 to produce the correct fan-out.
	//
	// Post-M0 deployment replicas have non-deterministic names
	// (`<app>.<replica_id>`), so the resolver can no longer
	// synthesize them from a count; it has to ask the runtime.
	Containers ContainerManager
}

// IngressStatus persists whatever the ingress plugin returned. As with
// DatabaseStatus, the Data map is the refLookup-visible shape.
type IngressStatus struct {
	Plugin string         `json:"plugin"`
	Data   map[string]any `json:"data,omitempty"`
}

func (h *IngressHandler) Handle(ctx context.Context, ev WatchEvent) error {
	switch ev.Type {
	case WatchPut:
		return h.apply(ctx, ev)
	case WatchDelete:
		return h.remove(ctx, ev)
	}

	return nil
}

func (h *IngressHandler) apply(ctx context.Context, ev WatchEvent) error {
	if ev.Manifest == nil {
		return fmt.Errorf("put event without manifest")
	}

	var spec ingressSpec
	if err := json.Unmarshal(ev.Manifest.Spec, &spec); err != nil {
		return fmt.Errorf("decode ingress spec: %w", err)
	}

	// ingress-per-app is the overwhelmingly common shape: one
	// `deployment "api"` paired with one `ingress "api"`. When the
	// ingress name matches the target deployment, the HCL is pure
	// boilerplate — so we default Service to the ingress name. Operators
	// who actually want a cross-app route (ingress "public" → deployment
	// "api") still declare `service = "..."` explicitly; we only fill the
	// blank, never override.
	if spec.Service == "" {
		spec.Service = ev.Name
	}

	if spec.Host == "" {
		return fmt.Errorf("ingress/%s: host is required", ev.Name)
	}

	up, err := h.resolveUpstream(ctx, ev.Scope, ev.Name, &spec)
	if err != nil {
		return err
	}

	pluginName := h.PluginName
	if pluginName == "" {
		pluginName = "caddy"
	}

	// Same AppID story as deployments: the router plugin keys its state
	// by the app name it receives (voodu-caddy writes routes/<app>.json),
	// so two scopes declaring `ingress "web"` would clobber each other's
	// file without the scope prefix.
	app := AppID(ev.Scope, ev.Name)

	env := ingressApplyEnv(app, spec, up)

	res, err := h.Invoker.Invoke(ctx, pluginName, "apply", []string{app}, env)
	if err != nil {
		return err
	}

	if res.ExitCode != 0 {
		return fmt.Errorf("%s apply exited %d: %s", pluginName, res.ExitCode, pluginErrorDetail(res))
	}

	status := IngressStatus{Plugin: pluginName}

	if res.Envelope != nil {
		if res.Envelope.Status == "error" {
			return fmt.Errorf("%s apply: %s", pluginName, res.Envelope.Error)
		}

		status.Data = envelopeDataAsMap(res.Envelope.Data)
	}

	if status.Data == nil {
		// Plugin chose plain-text or an empty envelope. Keep a minimal
		// Data shape so refLookup has something sensible to surface.
		status.Data = map[string]any{
			"host":    spec.Host,
			"service": spec.Service,
		}
	}

	blob, err := json.Marshal(status)
	if err != nil {
		return err
	}

	if err := h.Store.PutStatus(ctx, KindIngress, app, blob); err != nil {
		return err
	}

	h.logf("ingress/%s ready (host=%s → %s via %s)", ev.Name, spec.Host, spec.Service, pluginName)

	return nil
}

func (h *IngressHandler) remove(ctx context.Context, ev WatchEvent) error {
	app := AppID(ev.Scope, ev.Name)

	raw, err := h.Store.GetStatus(ctx, KindIngress, app)
	if err != nil {
		return err
	}

	if raw == nil {
		return nil
	}

	var status IngressStatus
	if err := json.Unmarshal(raw, &status); err != nil {
		return fmt.Errorf("decode ingress status: %w", err)
	}

	pluginName := status.Plugin
	if pluginName == "" {
		pluginName = "caddy"
	}

	res, err := h.Invoker.Invoke(ctx, pluginName, "remove", []string{app}, map[string]string{
		plugin.EnvApp: app,
	})
	if err != nil {
		return err
	}

	if res.ExitCode != 0 {
		return fmt.Errorf("%s remove exited %d: %s", pluginName, res.ExitCode, pluginErrorDetail(res))
	}

	if err := h.Store.DeleteStatus(ctx, KindIngress, app); err != nil {
		return err
	}

	h.logf("ingress/%s removed", ev.Name)

	return nil
}

// upstreamResolution is what resolveUpstream hands back: the list of
// `host:port` pairs caddy should balance across, plus any health-check
// path the deployment declared. Split out so ingressApplyEnv can be
// called from tests without reconstituting a full Store.
type upstreamResolution struct {
	Upstreams       []string
	HealthCheckPath string
}

// ingressApplyEnv packs the spec into the env the plugin receives. As
// with database, we favour env over positional args so new spec fields
// land without breaking older plugins that ignore them.
func ingressApplyEnv(name string, spec ingressSpec, up upstreamResolution) map[string]string {
	env := map[string]string{
		plugin.EnvApp:            name,
		plugin.EnvIngressHost:    spec.Host,
		plugin.EnvIngressService: spec.Service,
	}

	if spec.Port > 0 {
		env[plugin.EnvIngressPort] = fmt.Sprintf("%d", spec.Port)
	}

	// Multi-upstream path: when the deployment has replicas > 1 (or we
	// indexed a single-slot deployment), hand caddy the explicit list
	// so it load-balances instead of dialing a stale bare-name DNS.
	// Older plugin versions ignore this var and still work against the
	// SERVICE/PORT pair.
	if len(up.Upstreams) > 0 {
		if b, err := json.Marshal(up.Upstreams); err == nil {
			env[plugin.EnvIngressUpstreams] = string(b)
		}
	}

	if spec.LB != nil {
		if spec.LB.Policy != "" {
			env[plugin.EnvIngressLBPolicy] = spec.LB.Policy
		}

		if spec.LB.Interval != "" {
			env[plugin.EnvIngressLBInterval] = spec.LB.Interval
		}
	}

	if up.HealthCheckPath != "" {
		env[plugin.EnvIngressHealthCheckPath] = up.HealthCheckPath
	}

	if spec.TLS != nil {
		if spec.TLS.Enabled {
			env[plugin.EnvIngressTLS] = "true"
		}

		if spec.TLS.Provider != "" {
			env[plugin.EnvIngressTLSProvider] = spec.TLS.Provider
		}

		if spec.TLS.Email != "" {
			env[plugin.EnvIngressTLSEmail] = spec.TLS.Email
		}

		if spec.TLS.OnDemand {
			env[plugin.EnvIngressTLSOnDemand] = "true"
		}

		if spec.TLS.Ask != "" {
			env[plugin.EnvIngressTLSAsk] = spec.TLS.Ask
		}
	}

	// Path routing is opt-in; only set the env var when the operator
	// actually declared `location` blocks. An empty map entry would make
	// old plugin versions parse "" and error needlessly — skipping the
	// key preserves backward-compat with plugins predating locations.
	if len(spec.Locations) > 0 {
		// Marshal error is unreachable: spec.Locations is a slice of
		// primitive-typed structs. Ignore the error defensively rather
		// than propagate — the invoker would surface any real problem on
		// the plugin side when it fails to parse.
		if b, err := json.Marshal(spec.Locations); err == nil {
			env[plugin.EnvIngressLocations] = string(b)
		}
	}

	return env
}

// resolveUpstream validates that the target deployment actually exists
// in /desired and fills in spec.Port when the operator left it blank.
// Two goals:
//
//  1. Fail-fast on typos — applying an ingress that names a deployment
//     nobody applied produces a clean error instead of a mystery 502
//     at request time.
//  2. Let manifests omit `port` when the deployment already declares
//     it. Cuts redundancy from the common case.
//
// Precedence when port is missing:
//
//	spec.Port (explicit)
//	  > deployment "<name>".ports[0]  (container port, after `host:` split)
//	  > 80 (default)
//
// The 80 fallback keys off the fact that ingress is always HTTP
// routing (caddy-only) and every common base image that fronts a web
// app — caddy, nginx, httpd — listens there. Apps that actually use
// a different port (rails:3000, flask:5000) declare it explicitly.
// Getting 80 wrong doesn't fail silently — caddy-ingress surfaces
// `connection refused` immediately on the first request, which is
// faster to debug than a manifest that refuses to apply.
//
// Target-not-found is marked Transient: `voodu apply -f` may send
// ingress before the deployment it references, and the reconciler
// retries after backoff. Typos surface once the backoff log noise
// makes them obvious — still better than 502s in prod.
const defaultIngressPort = 80

// resolveUpstream validates the target exists and builds the upstream
// list for caddy. Deployments are scope-local: an ingress in scope X
// only resolves deployments declared under scope X. We look up the
// deployment spec to read its replica count and health_check path,
// then emit one upstream per slot via the runtime's container list.
//
// spec.Port is filled in from whichever source provides it (explicit >
// deployment.ports[0] > 80). See the port-fallback rationale above.
func (h *IngressHandler) resolveUpstream(ctx context.Context, scope, name string, spec *ingressSpec) (upstreamResolution, error) {
	dep, err := h.lookupDeploymentSpec(ctx, scope, spec.Service)
	if err != nil {
		return upstreamResolution{}, err
	}

	if dep == nil {
		return upstreamResolution{}, Transient(fmt.Errorf("ingress/%s: no deployment named %q yet — will retry", name, spec.Service))
	}

	if spec.Port == 0 {
		if port, ok := firstContainerPort(dep.Ports); ok {
			spec.Port = port
		} else {
			spec.Port = defaultIngressPort
		}
	}

	upstreams, err := h.deploymentUpstreams(scope, spec.Service, spec.Port, *dep)
	if err != nil {
		return upstreamResolution{}, err
	}

	return upstreamResolution{
		Upstreams:       upstreams,
		HealthCheckPath: dep.HealthCheck,
	}, nil
}

// deploymentUpstreams produces the upstream caddy will dial for
// this ingress. Returns a single `<alias>:<port>` entry — the
// network alias (e.g. "web.clowk-lp") that BuildNetworkAliases
// registered on every replica's docker --network-alias flag.
//
// Why the alias and not per-container names: container names are
// replica-id qualified ("clowk-lp-web.afd2") and the embedded dot
// trips docker's built-in DNS — the resolver treats `.afd2` as a
// search-domain segment and reports `no such host` even though
// the container is on the same network. The alias has no replica
// suffix and resolves to every live replica IP automatically,
// giving caddy DNS-level round-robin for free.
//
// Three retry-friendly cases the caller has to absorb:
//
//   - Containers nil — handler wired without a manager (tests).
//     Returns the bare alias anyway; tests that care about the
//     plugin contract still get a sensible value.
//   - List error — wrap as Transient so the reconciler retries.
//   - List returns zero containers — the deployment exists in
//     /desired but the reconciler hasn't created replicas yet
//     (the ingress event landed first). Same Transient retry,
//     because dialing the alias before any replica registers it
//     would produce the same DNS failure caddy is reporting.
func (h *IngressHandler) deploymentUpstreams(scope, name string, port int, dep deploymentSpec) ([]string, error) {
	app := AppID(scope, name)

	upstream := upstreamForAlias(scope, name, app, port)

	if h.Containers == nil {
		return []string{upstream}, nil
	}

	slots, err := h.Containers.ListByIdentity(string(KindDeployment), scope, name)
	if err != nil {
		return nil, Transient(fmt.Errorf("list deployment %s replicas: %w", app, err))
	}

	if len(slots) == 0 {
		return nil, Transient(fmt.Errorf("ingress: deployment %s has no live replicas yet — will retry", app))
	}

	return []string{upstream}, nil
}

// upstreamForAlias picks the canonical network alias (e.g.
// "web.clowk-lp") for a deployment so caddy dials a name docker's
// embedded DNS can resolve. Falls back to the bare AppID for
// unscoped resources or empty inputs (theoretical paths not used
// by any current handler — defensive only).
func upstreamForAlias(scope, name, app string, port int) string {
	aliases := BuildNetworkAliases(scope, name)

	if len(aliases) == 0 {
		return fmt.Sprintf("%s:%d", app, port)
	}

	return fmt.Sprintf("%s:%d", aliases[0], port)
}

func (h *IngressHandler) lookupDeploymentSpec(ctx context.Context, scope, name string) (*deploymentSpec, error) {
	m, err := h.Store.Get(ctx, KindDeployment, scope, name)
	if err != nil {
		return nil, err
	}

	if m == nil {
		return nil, nil
	}

	var spec deploymentSpec
	if err := json.Unmarshal(m.Spec, &spec); err != nil {
		return nil, fmt.Errorf("decode deployment/%s: %w", name, err)
	}

	return &spec, nil
}

// firstContainerPort pulls the first numeric port out of a deployment's
// `ports` slice. Accepts "3000", "8080:3000", and "3000/udp" — for the
// "host:container" form we take the container side, since ingress
// traffic stays inside the docker network and the host-published port
// is irrelevant there.
func firstContainerPort(ports []string) (int, bool) {
	for _, raw := range ports {
		p := strings.TrimSpace(raw)
		if p == "" {
			continue
		}

		if slash := strings.IndexByte(p, '/'); slash >= 0 {
			p = p[:slash]
		}

		if colon := strings.LastIndexByte(p, ':'); colon >= 0 {
			p = p[colon+1:]
		}

		if n, err := strconv.Atoi(p); err == nil && n > 0 {
			return n, true
		}
	}

	return 0, false
}

func (h *IngressHandler) logf(format string, args ...any) {
	if h.Log == nil {
		return
	}

	h.Log.Printf(format, args...)
}
