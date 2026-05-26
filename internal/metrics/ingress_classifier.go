package metrics

import (
	"slices"
	"strings"
)

// IsAssetRequest classifies whether a request URI corresponds to a
// static asset (JS bundle, image, font, CSS, source map, etc.)
// versus a meaningful "user-visible" hit (HTML page, API call,
// form submit).
//
// Used by IngressAggregator.Push to keep `req_count` /
// status-class counters / latency percentiles SCOPED TO REAL HITS.
// Without this filter, a single Next.js page navigation pushes
// 30–100+ rows through the log (HTML + JS chunks + CSS + fonts +
// `/_next/static/*` + service worker etc.), inflating req_count
// by ~50× and pulling p95 latency down to "asset cache hit" range
// (~1 ms) where it stops being useful.
//
// Bytes-out is NOT filtered: it stays a true total-bandwidth
// signal, including assets — the value operators want to size
// against egress/capacity.
//
// Heuristic, two signals:
//
//   1. Path prefix — frameworks dump assets under stable
//      directories. Catches the case where the actual file in the
//      URL has no extension (e.g. hashed chunk names produced
//      server-side, or Next.js `/_next/data/...json` payloads that
//      LOOK like API but are framework-generated).
//
//   2. Extension at the end of the path — the long tail of
//      individual asset files served from custom locations.
//
// Heuristic, not a parser — kept deliberately conservative so
// `/api/foo` (no extension) ALWAYS counts as a hit, even though
// the response is JSON. If a real API ever serves `.json` literally
// in the URL (e.g. `/data.json`), it'll be misclassified as asset;
// operator can rename or we add a config knob if it ever bites.
func IsAssetRequest(uri string) bool {
	if uri == "" {
		return false
	}

	// Strip query string + fragment so `?v=123` on a bundle URL
	// doesn't trip the extension check.
	path := uri
	if i := strings.IndexAny(path, "?#"); i >= 0 {
		path = path[:i]
	}

	for _, prefix := range assetPathPrefixes {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}

	// Find the last dot AFTER the last slash, so we're looking at
	// the actual file extension not a dot earlier in the path.
	slash := strings.LastIndexByte(path, '/')
	if slash < 0 {
		slash = -1
	}
	dot := strings.LastIndexByte(path[slash+1:], '.')
	if dot < 0 {
		return false
	}
	ext := strings.ToLower(path[slash+1+dot:])

	return slices.Contains(assetExtensions, ext)
}

// assetPathPrefixes — directories that frameworks reliably use
// for static asset output. Match against the request path, NOT
// the host (host is the per-deployment routing key).
//
// New frameworks: add a prefix here. Operators with non-standard
// layouts can rename their static dir or — if it becomes a recurring
// ask — we accept a config knob to extend the list.
var assetPathPrefixes = []string{
	"/_next/",         // Next.js (covers /_next/static/ + /_next/data/ + HMR)
	"/_nuxt/",         // Nuxt 3
	"/_remix/",        // Remix (rare; usually under /build/)
	"/static/",        // Generic + Django + Flask conventions
	"/assets/",        // Rails asset pipeline (also Vite output default)
	"/public/",        // Some Express / Hono setups
	"/build/",         // Remix, Vite, esbuild defaults
	"/dist/",          // Bundler outputs occasionally served directly
	"/.well-known/",   // ACME / security.txt / discovery files
	"/__webpack_hmr",  // Webpack dev server HMR endpoint
	"/favicon.ico",    // Always asset, often served as 404 on missing
	"/robots.txt",     // Crawler files, not user hits
	"/sitemap.xml",    // Same
	// Specific filenames (not directories) operators repeatedly hit
	// without it being a "real" user request:
	"/manifest.json",  // PWA manifest — extension `.json` is left out
	                   // of the generic list (would also kill Rails-
	                   // style API URLs like `/users.json`), so the
	                   // PWA file is pinned by full name here instead.
}

// assetExtensions — file types overwhelmingly served as static
// assets. Lowercased; the classifier lowercases the URL extension
// before comparing, so case-insensitive.
//
// `.json` is INTENTIONALLY ABSENT — APIs serve JSON without a `.json`
// suffix; treating any `.json` URL as asset would skip Next.js
// `/_next/data/...json` (covered by prefix above) but also miss
// legitimate `/data.json` API endpoints. The prefix list covers the
// known framework cases; the extension fallback stays focused on
// file types that are always assets.
var assetExtensions = []string{
	".js", ".mjs", ".cjs",
	".css",
	".map",
	".png", ".jpg", ".jpeg", ".gif", ".svg", ".ico", ".webp", ".avif", ".bmp", ".heic", ".heif",
	".woff", ".woff2", ".ttf", ".otf", ".eot",
	".mp4", ".webm", ".ogg", ".mp3", ".wav",
	".m3u8", ".mpd", // HLS / DASH video manifests (NOT .ts — collides
	                 // with TypeScript source in dev builds; HLS segment
	                 // misses are acceptable, they're rare alone)
	".vtt", // WebVTT subtitles
	".webmanifest",          // PWA manifest alternative name
	".pdf",
	".txt",
}
