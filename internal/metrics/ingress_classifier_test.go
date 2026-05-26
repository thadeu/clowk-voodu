package metrics

import "testing"

func TestIsAssetRequest_FrameworkPrefixes(t *testing.T) {
	cases := []struct {
		uri  string
		want bool
		why  string
	}{
		// Next.js
		{"/_next/static/chunks/main-abc123.js", true, "next chunk"},
		{"/_next/data/123/page.json", true, "next data payload"},
		{"/_next/image?url=...&w=640&q=75", true, "next image proxy"},
		// Nuxt
		{"/_nuxt/entry.abc.js", true, "nuxt entry"},
		// Generic
		{"/static/css/site.abc.css", true, "django/flask static"},
		{"/assets/application-abc.js", true, "rails sprockets"},
		{"/public/logo.png", true, "express/hono public"},
		{"/build/manifest.json", true, "remix/vite build dir"},
		// Crawler / browser cache files
		{"/favicon.ico", true, "favicon"},
		{"/robots.txt", true, "robots"},
		{"/sitemap.xml", true, "sitemap (crawler file, not user hit)"},
	}
	for _, c := range cases {
		got := IsAssetRequest(c.uri)
		if got != c.want {
			t.Errorf("IsAssetRequest(%q) = %v, want %v (%s)", c.uri, got, c.want, c.why)
		}
	}
}

func TestIsAssetRequest_ExtensionFallback(t *testing.T) {
	assetURIs := []string{
		"/bundle.js",
		"/styles.css",
		"/logo.png",
		"/photo.jpg",
		"/icon.svg",
		"/Font.WOFF2", // case-insensitive
		"/path/to/file.map",
		"/video.mp4",
		"/audio.mp3",
		"/doc.pdf",
	}
	for _, uri := range assetURIs {
		if !IsAssetRequest(uri) {
			t.Errorf("IsAssetRequest(%q) = false, want true (asset extension)", uri)
		}
	}
}

// TestIsAssetRequest_RealNextjsPageView pins the exact URIs an
// operator observed Next.js producing for a single page navigation
// (logged 2026-05-25). All but `/` MUST classify as asset so the
// counter reflects "1 page view = 1 hit".
func TestIsAssetRequest_RealNextjsPageView(t *testing.T) {
	pageHits := []string{
		"/", // the only true page view
	}
	assetHits := []string{
		"/clowk-white.svg",
		"/_next/static/chunks/0d-g9_qd4.5uk.css",
		"/manifest.json", // PWA — pinned by full name in prefix list
		"/_next/static/chunks/090na0~ec91zy.css",
		"/_next/static/chunks/turbopack-084xukuyapalm.js",
		"/_next/static/chunks/0ieksyay9_zik.js",
		"/videos/demo-poster.jpg",
		"/__next._head.txt?_rsc=1pn8p",      // RSC stream, .txt
		"/__next._index.txt?_rsc=nn07o",     // RSC stream, .txt
		"/__next.__PAGE__.txt?_rsc=ivliq",   // RSC stream, .txt
		"/videos/demo.m3u8",                  // HLS — added to ext list
	}

	for _, uri := range pageHits {
		if IsAssetRequest(uri) {
			t.Errorf("page URI %q misclassified as asset", uri)
		}
	}
	for _, uri := range assetHits {
		if !IsAssetRequest(uri) {
			t.Errorf("asset URI %q misclassified as page (would inflate counter)", uri)
		}
	}
}

func TestIsAssetRequest_NotAssets(t *testing.T) {
	pageOrAPI := []string{
		"/",                          // homepage
		"/pricing",                   // page
		"/products/abc-123",          // slug page
		"/api/users",                 // API
		"/api/v1/orders/42",          // versioned API
		"/login",                     // page
		"/data.json",                 // INTENTIONAL: .json not in extension list
		"/health",                    // health check (counts as hit)
		"/users/show?id=42&full=1",   // query string ignored
		"/dashboard#section",         // fragment ignored
	}
	for _, uri := range pageOrAPI {
		if IsAssetRequest(uri) {
			t.Errorf("IsAssetRequest(%q) = true, want false (page/API)", uri)
		}
	}
}

func TestIsAssetRequest_QueryAndFragmentStripped(t *testing.T) {
	// Asset with cache-busting query — must still classify as asset
	if !IsAssetRequest("/static/app.js?v=123abc") {
		t.Error("asset with ?v= query should still classify as asset")
	}
	if !IsAssetRequest("/logo.png#hash") {
		t.Error("asset with #hash should still classify as asset")
	}
	if !IsAssetRequest("/bundle.js?utm_source=x&v=1#section") {
		t.Error("asset with mixed query + fragment should still classify as asset")
	}
}

func TestIsAssetRequest_EdgeCases(t *testing.T) {
	if IsAssetRequest("") {
		t.Error("empty URI should NOT be asset")
	}
	if IsAssetRequest("/") {
		t.Error("root path should NOT be asset")
	}
	// Path with dot in directory but no actual extension at end
	if IsAssetRequest("/v1.0/api/users") {
		t.Error("/v1.0/api/users — dot is in a path segment, not the file ext")
	}
}

func TestAggregator_PushSkipsAssetsForCount(t *testing.T) {
	a := NewIngressAggregator()

	// 1 page request, 10 assets
	a.Push("app.com", "web", "main", IngressRequest{URI: "/", Status: 200, DurationMs: 120, SizeBytes: 5000})
	for range 10 {
		a.Push("app.com", "web", "main", IngressRequest{URI: "/_next/static/chunks/abc.js", Status: 200, DurationMs: 2, SizeBytes: 10000})
	}

	buckets := a.Drain()
	if len(buckets) != 1 {
		t.Fatalf("expected 1 bucket, got %d", len(buckets))
	}

	var b *ingressBucket
	for _, v := range buckets {
		b = v
	}

	// count + 2xx should be 1 (only the page hit)
	if b.count != 1 {
		t.Errorf("count = %d, want 1 (assets skipped)", b.count)
	}
	if b.s2xx != 1 {
		t.Errorf("s2xx = %d, want 1 (assets skipped from status breakdown)", b.s2xx)
	}

	// bytesOut should sum EVERYTHING (page + assets) — that's bandwidth
	wantBytes := uint64(5000 + 10*10000)
	if b.bytesOut != wantBytes {
		t.Errorf("bytesOut = %d, want %d (assets DO count toward bandwidth)", b.bytesOut, wantBytes)
	}

	// durations should only include the page (assets skipped)
	if len(b.durations) != 1 {
		t.Errorf("durations len = %d, want 1 (assets skipped from latency)", len(b.durations))
	}
}
