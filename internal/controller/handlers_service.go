package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

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
	Host    string       `json:"host"`
	Service string       `json:"service"`
	Port    int          `json:"port,omitempty"`
	TLS     *ingressTLS  `json:"tls,omitempty"`
}

type ingressTLS struct {
	Enabled  bool   `json:"enabled,omitempty"`
	Provider string `json:"provider,omitempty"`
	Email    string `json:"email,omitempty"`
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

	if spec.Host == "" || spec.Service == "" {
		return fmt.Errorf("ingress/%s: host and service are required", ev.Name)
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
		return fmt.Errorf("%s apply exited %d: %s", pluginName, res.ExitCode, string(res.Stderr))
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
		return fmt.Errorf("%s remove exited %d: %s", pluginName, res.ExitCode, string(res.Stderr))
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
	}

	return env
}

func (h *IngressHandler) logf(format string, args ...any) {
	if h.Log == nil {
		return
	}

	h.Log.Printf(format, args...)
}
