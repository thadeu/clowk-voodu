# Gokku Documentation

This directory contains the official Gokku documentation, built with [VitePress](https://vitepress.dev/).

## Local Development

Install dependencies:

```bash
npm install
```

Start dev server:

```bash
npm run docs:dev
```

Visit http://localhost:5173

## Build

Build static site:

```bash
npm run docs:build
```

Preview build:

```bash
npm run docs:preview
```

## Deployment

Documentation is automatically deployed to GitHub Pages on push to `main` branch.

See `.github/workflows/deploy.yml` for CI/CD configuration.

## Structure

```
docs/
├── .vitepress/
│   └── config.mts         # VitePress configuration
├── guide/                  # User guides
├── examples/              # Usage examples
├── reference/             # API/config reference
└── index.md               # Homepage
```

## Contributing

1. Edit markdown files in `docs/`
2. Test locally with `npm run docs:dev`
3. Commit and push
4. Docs auto-deploy to GitHub Pages

## Links

- [VitePress Documentation](https://vitepress.dev/)
- [GitHub Pages](https://pages.github.com/)

