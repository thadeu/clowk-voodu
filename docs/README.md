# docs/ — voodu landing page + documentation

Next.js + Fumadocs site for [Voodu](https://voodu.clowk.in). Lives in the
same repo as the controller so doc changes ship in the same PR as the
code change that motivated them.

## Stack

- Next.js 16 (static export → `out/`)
- React 19
- Tailwind CSS 4
- Fumadocs (UI + MDX) with custom dark theme
- Cloudflare Pages (CDN + edge cache)

## Development

```bash
cd docs
pnpm install
pnpm dev
```

Open [http://localhost:3000](http://localhost:3000).

## Build

```bash
cd docs
pnpm build
```

Static output → `out/`. The `install` script from the repo root must
be copied into `public/install` before build so it lands at
`out/install` after — that's how `voodu.clowk.in/install | bash`
works.

```bash
cp ../install public/install
pnpm build
```

`.github/workflows/docs.yml` does this automatically on push to `main`.

## Deploy

Automatic via GitHub Actions on push to `main` (when `docs/**` or
`install` changed). The workflow:

1. Installs deps with pnpm
2. Copies `install` → `docs/public/install`
3. `pnpm build`
4. `wrangler pages deploy docs/out --project-name voodu-lp`

DNS at `voodu.clowk.in` points at the Cloudflare Pages project.

## Layout

```
app/                   ← Next App Router
  layout.tsx           ← global layout, IBM Plex fonts, metadata
  page.tsx             ← landing
  not-found.tsx
  docs/
    layout.tsx         ← Fumadocs DocsLayout + custom sidebar
    [[...slug]]/page.tsx
components/
  Header / Hero / HeroTerminal / InstallShell
  Strip / HCLBlock / HowItWorks / CLICheats / Stack / FAQ / EndCTA / Footer
  mdx.tsx              ← shared MDX components
  docs/
    docs-sidebar.tsx   ← collapsible sidebar with active scroll-into-view
    sidebar-content.ts ← sections + items + lucide icons
content/docs/          ← all .mdx + meta.json files
lib/source.ts          ← Fumadocs loader
public/                ← manifest.json, llm.txt, icons, install (copied at build)
```
