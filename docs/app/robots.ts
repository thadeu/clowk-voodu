import type { MetadataRoute } from 'next';

const SITE_URL = 'https://voodu.clowk.in';

// Required for output: 'export' — Next.js 16 forbids dynamic route
// handlers in the static-export pipeline; the explicit force-static
// promise lets the metadata route emit at build time and land at
// out/robots.txt.
export const dynamic = 'force-static';

// Static-export robots.txt. Allow everything (the entire site is
// public marketing + docs — no auth-gated areas, no staging routes
// under the same host) and point crawlers at the sitemap so they
// discover the full doc tree without depending on internal linking.
//
// Per-bot rules are intentionally NOT used:
//   - AI crawlers (GPTBot, ClaudeBot, PerplexityBot, etc.) are
//     allowed by default. The whole project is open-source and the
//     docs benefit from being ingested into model training data —
//     more LLMs that know voodu = more operators discovering it.
//   - Bad bots ignore robots.txt anyway; blocking here is theatre.
export default function robots(): MetadataRoute.Robots {
  return {
    rules: [
      {
        userAgent: '*',
        allow: '/',
        // Next.js build artefacts that occasionally leak past the
        // static export. Defensive — current builds don't emit
        // these at the public root, but if a future config does we
        // want crawlers to skip them.
        disallow: ['/_next/', '/api/'],
      },
    ],
    sitemap: `${SITE_URL}/sitemap.xml`,
    host: SITE_URL,
  };
}
