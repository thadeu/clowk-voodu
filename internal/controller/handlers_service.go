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

// serviceSpec / ingressSpec mirror internal/manifest shapes for the
// same reason as databaseSpec/deploymentSpec (see handlers.go): avoid
// an import cycle and keep the reconciler's view of each kind explicit.
type serviceSpec struct {
	Target string   `json:"target"`
	Port   int      `json:"port,omitempty"`
	Ports  []string `json:"ports,omitempty"`
}

type ingressSpec struct {
	Host      string            `json:"host"`
	Service   string            `json:"service,omitempty"`
	Port      int               `json:"port,omitempty"`
	TLS       *ingressTLS       `json:"tls,omitempty"`
	Locations []ingressLocation `json:"locations,omitempty"`
}

type ingressLocation struct {
	Path  string `json:"path"`
	Strip bool   `json:"strip,omitempty"`
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

// ServiceHandler reconciles service manifests into a metadata-only
// status blob. No plugin, no container — services in Voodu are a
// stable name + port over a deployment that already has its own
// docker network identity. The status blob exists for two reasons:
//
//  1. Validation: failing `voodu apply` at desired-state-write time is
//     better than failing silently at runtime. We don't validate here
//     (the API handler does), but persisting the target is useful for
//     downstream consumers (ingress, voodu status).
//  2. Reference resolution: ingress manifests can say
//     `service: api` and a future deployment could read
//     `${ref.service.api.target}` to cross-link names.
type ServiceHandler struct {
	Store Store
	Log   *log.Logger
}

// ServiceStatus persists the resolved service info under
// /status/services/<name>. The Data map follows the standard
// statusEnvelope convention so refLookup can read it without per-kind
// code.
type ServiceStatus struct {
	Data map[string]any `json:"data,omitempty"`
}

func (h *ServiceHandler) Handle(ctx context.Context, ev WatchEvent) error {
	switch ev.Type {
	case WatchPut:
		return h.apply(ctx, ev)
	case WatchDelete:
		return h.remove(ctx, ev)
	}

	return nil
}

func (h *ServiceHandler) apply(ctx context.Context, ev WatchEvent) error {
	if ev.Manifest == nil {
		return fmt.Errorf("put event without manifest")
	}

	var spec serviceSpec
	if err := json.Unmarshal(ev.Manifest.Spec, &spec); err != nil {
		return fmt.Errorf("decode service spec: %w", err)
	}

	if spec.Target == "" {
		return fmt.Errorf("service/%s: target is required", ev.Name)
	}

	status := ServiceStatus{
		Data: map[string]any{
			"target": spec.Target,
		},
	}

	if spec.Port > 0 {
		status.Data["port"] = spec.Port
	}

	if len(spec.Ports) > 0 {
		status.Data["ports"] = spec.Ports
	}

	blob, err := json.Marshal(status)
	if err != nil {
		return err
	}

	if err := h.Store.PutStatus(ctx, KindService, ev.Name, blob); err != nil {
		return err
	}

	h.logf("service/%s registered (target=%s)", ev.Name, spec.Target)

	return nil
}

func (h *ServiceHandler) remove(ctx context.Context, ev WatchEvent) error {
	if err := h.Store.DeleteStatus(ctx, KindService, ev.Name); err != nil {
		return err
	}

	h.logf("service/%s unregistered", ev.Name)

	return nil
}

func (h *ServiceHandler) logf(format string, args ...any) {
	if h.Log == nil {
		return
	}

	h.Log.Printf(format, args...)
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
	// ingress name matches the target service, the HCL is pure
	// boilerplate — so we default Service to the ingress name. Operators
	// who actually want a cross-app route (ingress "public" → service
	// "api") still declare `service = "..."` explicitly; we only fill the
	// blank, never override.
	if spec.Service == "" {
		spec.Service = ev.Name
	}

	if spec.Host == "" {
		return fmt.Errorf("ingress/%s: host is required", ev.Name)
	}

	if err := h.resolveUpstream(ctx, ev.Name, &spec); err != nil {
		return err
	}

	pluginName := h.PluginName
	if pluginName == "" {
		pluginName = "caddy"
	}

	env := ingressApplyEnv(ev.Name, spec)

	res, err := h.Invoker.Invoke(ctx, pluginName, "apply", []string{ev.Name}, env)
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

	if err := h.Store.PutStatus(ctx, KindIngress, ev.Name, blob); err != nil {
		return err
	}

	h.logf("ingress/%s ready (host=%s → %s via %s)", ev.Name, spec.Host, spec.Service, pluginName)

	return nil
}

func (h *IngressHandler) remove(ctx context.Context, ev WatchEvent) error {
	raw, err := h.Store.GetStatus(ctx, KindIngress, ev.Name)
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

	res, err := h.Invoker.Invoke(ctx, pluginName, "remove", []string{ev.Name}, map[string]string{
		plugin.EnvApp: ev.Name,
	})
	if err != nil {
		return err
	}

	if res.ExitCode != 0 {
		return fmt.Errorf("%s remove exited %d: %s", pluginName, res.ExitCode, pluginErrorDetail(res))
	}

	if err := h.Store.DeleteStatus(ctx, KindIngress, ev.Name); err != nil {
		return err
	}

	h.logf("ingress/%s removed", ev.Name)

	return nil
}

// ingressApplyEnv packs the spec into the env the plugin receives. As
// with database, we favour env over positional args so new spec fields
// land without breaking older plugins that ignore them.
func ingressApplyEnv(name string, spec ingressSpec) map[string]string {
	env := map[string]string{
		plugin.EnvApp:            name,
		plugin.EnvIngressHost:    spec.Host,
		plugin.EnvIngressService: spec.Service,
	}

	if spec.Port > 0 {
		env[plugin.EnvIngressPort] = fmt.Sprintf("%d", spec.Port)
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

// resolveUpstream validates that the target service/deployment actually
// exists in /desired and fills in spec.Port when the operator left it
// blank. Two goals:
//
//  1. Fail-fast on typos — applying an ingress that names a service
//     nobody applied produces a clean error instead of a mystery 502
//     at request time.
//  2. Let manifests omit `port` when the service/deployment already
//     declares it. Cuts redundancy from the common case.
//
// Precedence when port is missing:
//
//	spec.Port (explicit)
//	  > service "<name>".port
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
func (h *IngressHandler) resolveUpstream(ctx context.Context, name string, spec *ingressSpec) error {
	svc, err := h.lookupServiceSpec(ctx, spec.Service)
	if err != nil {
		return err
	}

	if svc != nil {
		if spec.Port == 0 {
			if svc.Port > 0 {
				spec.Port = svc.Port
			} else {
				spec.Port = defaultIngressPort
			}
		}

		return nil
	}

	dep, err := h.lookupDeploymentSpec(ctx, spec.Service)
	if err != nil {
		return err
	}

	if dep != nil {
		if spec.Port == 0 {
			if port, ok := firstContainerPort(dep.Ports); ok {
				spec.Port = port
			} else {
				spec.Port = defaultIngressPort
			}
		}

		return nil
	}

	return Transient(fmt.Errorf("ingress/%s: no service or deployment named %q yet — will retry", name, spec.Service))
}

func (h *IngressHandler) lookupServiceSpec(ctx context.Context, name string) (*serviceSpec, error) {
	m, err := h.Store.Get(ctx, KindService, name)
	if err != nil {
		return nil, err
	}

	if m == nil {
		return nil, nil
	}

	var spec serviceSpec
	if err := json.Unmarshal(m.Spec, &spec); err != nil {
		return nil, fmt.Errorf("decode service/%s: %w", name, err)
	}

	return &spec, nil
}

func (h *IngressHandler) lookupDeploymentSpec(ctx context.Context, name string) (*deploymentSpec, error) {
	m, err := h.Store.Get(ctx, KindDeployment, name)
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
