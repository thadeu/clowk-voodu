import type { MetadataRoute } from 'next';
import { source } from '@/lib/source';

const SITE_URL = 'https://voodu.clowk.in';

// See app/robots.ts for the force-static rationale.
export const dynamic = 'force-static';

// Static-export sitemap. Next.js runs this at build time and emits
// out/sitemap.xml — same lifecycle as the static MDX pages. With
// trailingSlash: true, every URL needs the trailing slash so search
// engines don't see them as duplicates of the redirect target.
//
// Why static priorities / changeFrequency: Google ignores both in
// practice but Bing / Yandex still honour them. Cheap to emit.
export default function sitemap(): MetadataRoute.Sitemap {
  const now = new Date();

  const staticRoutes: MetadataRoute.Sitemap = [
    {
      url: `${SITE_URL}/`,
      lastModified: now,
      changeFrequency: 'weekly',
      priority: 1.0,
    },
    {
      url: `${SITE_URL}/terms/`,
      lastModified: now,
      changeFrequency: 'yearly',
      priority: 0.3,
    },
  ];

  const docsRoutes: MetadataRoute.Sitemap = source.generateParams().map((p) => {
    const segments = (p.slug as string[] | undefined) ?? [];
    const path = segments.length === 0 ? '/docs/' : `/docs/${segments.join('/')}/`;

    return {
      url: `${SITE_URL}${path}`,
      lastModified: now,
      changeFrequency: 'weekly' as const,
      // Boost the entry-points; everything else stays at the default
      // mid-tier so search engines don't think we're trying to game
      // the ranking. Real signal comes from inbound links + content.
      priority: segments[0] === 'getting-started' ? 0.9 : 0.7,
    };
  });

  return [...staticRoutes, ...docsRoutes];
}
