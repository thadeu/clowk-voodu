// handlers_pat_plugin_proxy.go adds the plugin route plane to the PAT
// surface (HP0). A plugin whose manifest declares a `routes` block
// (see pkg/plugin.RouteConfig) gets a reverse-proxied path under the
// authenticated PAT listener:
//
//	/api/pat/v1/<prefix>/<scope>/<name>/<rest...>
//	  → PAT auth (read scope)
//	  → resolve the (scope, name) container's voodu0 IP
//	  → proxy to http://<ip>:<port>/<rest>, preserving method/query/body
//
// The upstream port is never published to the host: the container stays
// internal on voodu0, and the controller — which runs on the host and
// therefore cannot use docker DNS names, only IPs — bridges to it. This
// is what lets the WebUI reach a plugin's API (e.g. clowk-hep3's capture
// REST) through one authenticated door without opening a firewall port.
//
// Integration is a FRONT LAYER, not another mux route: withPluginRoutes
// wraps the PAT mux and intercepts only paths that both match the
// plugin-route shape AND resolve to a declared, non-reserved prefix.
// Everything else falls through to the existing mux untouched — so there
// is zero risk of a net/http ServeMux pattern conflict with the core
// routes, and an unknown path still 404s exactly as before.

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"

	"go.voodu.clowk.in/internal/docker"
	"go.voodu.clowk.in/internal/plugins"
)

// patBase is the PAT plane path prefix every route shares.
const patBase = "/api/pat/v1/"

// reservedPATPrefixes are the first path segments owned by core PAT
// routes. A plugin route may not use one of these — guarding here means
// a misconfigured plugin can never shadow a built-in endpoint.
var reservedPATPrefixes = map[string]struct{}{
	"stats":   {},
	"system":  {},
	"pods":    {},
	"logs":    {},
	"metrics": {},
	"pats":    {},
}

// pluginRoute is a resolved route declaration. The actual upstream port
// is computed at request time (see resolveUpstreamPort): a fixed Port, or
// dynamically from the resource spec's PortEnv var.
type pluginRoute struct {
	Prefix  string
	Port    int
	PortEnv string
}

// PluginRouteLookup resolves a URL prefix segment to a plugin's declared
// route. Behind an interface so tests inject a fake instead of scanning
// a real plugins directory.
type PluginRouteLookup interface {
	LookupRoute(prefix string) (pluginRoute, bool)
}

// ResourceIPResolver resolves a resource's (scope, name) to the voodu0
// IP of its running container. Behind an interface so tests avoid
// shelling out to docker. (Distinct from the probe package's
// ContainerIPResolver, which maps a single container name → IP; this one
// maps the higher-level (scope, name) → the resource's pod → IP.)
type ResourceIPResolver interface {
	ResolveIP(scope, name string) (string, error)
}

// withPluginRoutes wraps the PAT mux so plugin routes are intercepted
// (with PAT auth) before the mux sees them. When the proxy seams aren't
// wired (minimal test APIs), it returns next unchanged.
func (a *API) withPluginRoutes(next http.Handler, auth *patAuthorizer) http.Handler {
	if a.PluginRoutes == nil || a.ContainerIPs == nil {
		return next
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		prefix, scope, name, rest, ok := parsePluginRoutePath(r.URL.Path)
		if ok && !isReservedPATPrefix(prefix) {
			if route, found := a.PluginRoutes.LookupRoute(prefix); found {
				auth.Middleware(ScopeRead, func(w http.ResponseWriter, r *http.Request) {
					a.proxyToPlugin(w, r, route, scope, name, rest)
				})(w, r)

				return
			}
		}

		next.ServeHTTP(w, r)
	})
}

// parsePluginRoutePath splits a PAT path into the plugin-route shape
// /api/pat/v1/<prefix>/<scope>/<name>/<rest...>. rest is "/" when the
// path ends at <name>. ok is false when the path isn't under the PAT
// base or has fewer than three segments after it.
func parsePluginRoutePath(p string) (prefix, scope, name, rest string, ok bool) {
	if !strings.HasPrefix(p, patBase) {
		return "", "", "", "", false
	}

	segs := strings.SplitN(strings.TrimPrefix(p, patBase), "/", 4)
	if len(segs) < 3 || segs[0] == "" || segs[1] == "" || segs[2] == "" {
		return "", "", "", "", false
	}

	rest = "/"
	if len(segs) == 4 && segs[3] != "" {
		rest = "/" + segs[3]
	}

	return segs[0], segs[1], segs[2], rest, true
}

func isReservedPATPrefix(prefix string) bool {
	_, ok := reservedPATPrefixes[prefix]

	return ok
}

// proxyToPlugin resolves the target container + port and reverse-proxies
// the request to it.
func (a *API) proxyToPlugin(w http.ResponseWriter, r *http.Request, route pluginRoute, scope, name, upstreamPath string) {
	port, err := a.resolveUpstreamPort(r.Context(), route, scope, name)
	if err != nil {
		writeErr(w, http.StatusBadGateway, fmt.Errorf("plugin route %s: %w", route.Prefix, err))

		return
	}

	ip, err := a.ContainerIPs.ResolveIP(scope, name)
	if err != nil {
		writeErr(w, http.StatusBadGateway, fmt.Errorf("plugin route %s: resolve %s/%s: %w", route.Prefix, scope, name, err))

		return
	}

	target := &url.URL{
		Scheme: "http",
		Host:   net.JoinHostPort(ip, strconv.Itoa(port)),
	}

	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = target.Scheme
			pr.Out.URL.Host = target.Host
			pr.Out.URL.Path = upstreamPath
			pr.Out.URL.RawQuery = r.URL.RawQuery
			pr.Out.Host = target.Host
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			writeErr(w, http.StatusBadGateway, fmt.Errorf("plugin route %s: upstream %s: %w", route.Prefix, target.Host, err))
		},
	}

	proxy.ServeHTTP(w, r)
}

// dirPluginRouteLookup is the production PluginRouteLookup: it scans the
// plugins directory and matches a manifest's route prefix (defaulting to
// the plugin name). The scan only runs for non-reserved, plugin-shaped
// paths, so core PAT routes never pay for it.
type dirPluginRouteLookup struct {
	Root string
}

func (d dirPluginRouteLookup) LookupRoute(prefix string) (pluginRoute, bool) {
	if d.Root == "" {
		return pluginRoute{}, false
	}

	all, _ := plugins.LoadAll(d.Root)

	for _, p := range all {
		rc := p.Manifest.Routes

		// A usable route needs SOME way to find the port: a fixed Port
		// or a PortEnv to read from the resource spec.
		if rc == nil || (rc.Port <= 0 && rc.PortEnv == "") {
			continue
		}

		declared := rc.Prefix
		if declared == "" {
			declared = p.Manifest.Name
		}

		if strings.Trim(declared, "/") == prefix {
			return pluginRoute{Prefix: prefix, Port: rc.Port, PortEnv: rc.PortEnv}, true
		}
	}

	return pluginRoute{}, false
}

// resolveUpstreamPort computes the container port to proxy to. PortEnv
// (dynamic, read from the resource's expanded spec) wins over a fixed
// Port. This is what lets `api_port` in the HCL block flow through to the
// proxy without a static plugin.yml value to keep in sync.
func (a *API) resolveUpstreamPort(ctx context.Context, route pluginRoute, scope, name string) (int, error) {
	if route.PortEnv != "" {
		if port := a.portFromResourceEnv(ctx, scope, name, route.PortEnv); port > 0 {
			return port, nil
		}

		if route.Port > 0 {
			return route.Port, nil // fall back to a fixed port if declared
		}

		return 0, fmt.Errorf("could not resolve %s from %s/%s spec", route.PortEnv, scope, name)
	}

	if route.Port > 0 {
		return route.Port, nil
	}

	return 0, fmt.Errorf("route declares neither port nor port_env")
}

// portFromResourceEnv reads the resource's expanded spec from the store
// and extracts the port from the named env var (e.g. HEP_API_ADDR →
// "0.0.0.0:8080" → 8080). The plugin's kind expands to a core kind, so we
// try statefulset then deployment.
func (a *API) portFromResourceEnv(ctx context.Context, scope, name, key string) int {
	if a.Store == nil {
		return 0
	}

	for _, k := range []Kind{KindStatefulset, KindDeployment} {
		m, err := a.Store.Get(ctx, k, scope, name)
		if err != nil || m == nil {
			continue
		}

		if port := portFromSpecEnv(m.Spec, key); port > 0 {
			return port
		}
	}

	return 0
}

// portFromSpecEnv digs an env var's port out of a raw manifest spec.
func portFromSpecEnv(rawSpec json.RawMessage, key string) int {
	if len(rawSpec) == 0 {
		return 0
	}

	var s struct {
		Env map[string]any `json:"env"`
	}

	if err := json.Unmarshal(rawSpec, &s); err != nil {
		return 0
	}

	addr, _ := s.Env[key].(string)

	return portFromHostPort(addr)
}

// portFromHostPort parses the trailing port of a "host:port" string.
func portFromHostPort(addr string) int {
	idx := strings.LastIndex(addr, ":")
	if idx < 0 {
		return 0
	}

	n, err := strconv.Atoi(strings.TrimSpace(addr[idx+1:]))
	if err != nil {
		return 0
	}

	return n
}

// dockerResourceIPResolver is the production ResourceIPResolver: it
// finds the pod for (scope, name) via the lister, then reads its voodu0
// IP via docker inspect. Only deployment/statefulset pods are eligible —
// jobs/cronjobs are transient and don't host a long-lived API.
type dockerResourceIPResolver struct {
	Pods PodsLister
}

func (d dockerResourceIPResolver) ResolveIP(scope, name string) (string, error) {
	lister := d.Pods
	if lister == nil {
		lister = DockerPodsLister{}
	}

	pods, err := lister.ListPods()
	if err != nil {
		return "", fmt.Errorf("list pods: %w", err)
	}

	for i := range pods {
		p := pods[i]

		if p.Scope != scope || p.ResourceName != name {
			continue
		}

		if p.Kind != string(KindDeployment) && p.Kind != string(KindStatefulset) {
			continue
		}

		if !p.Running {
			continue
		}

		ip, err := docker.ContainerIP(p.Name)
		if err != nil {
			return "", fmt.Errorf("inspect %s: %w", p.Name, err)
		}

		if ip == "" {
			return "", fmt.Errorf("container %s has no voodu0 IP", p.Name)
		}

		return ip, nil
	}

	return "", fmt.Errorf("no running container for %s/%s", scope, name)
}
