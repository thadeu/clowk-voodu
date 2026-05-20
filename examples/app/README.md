# app/

The `app` block is sugar for a `deployment + ingress` pair with
the same identity. Every deployment-side feature voodu has lives
on this block too — adding ingress fields (`host`, `tls`,
`location`, `lb`) at the bottom doesn't cost you any of the
deployment knobs.

## Feature parity with `deployment`

| feature | works on `app`? |
|---|---|
| `image` / `build {}` | ✓ |
| `replicas` / `autoscale {}` (mutex) | ✓ |
| `env`, `env_file`, `env_from` | ✓ |
| `ports`, `volumes`, `networks` | ✓ |
| `extra_hosts`, `cap_add` | ✓ |
| `resources { limits { cpu memory } }` | ✓ |
| `logs { max_size max_files }` | ✓ |
| `release {}` | ✓ |
| `on_deploy {}` (block form + method + headers + body inline / file asset) | ✓ |
| `probes { liveness readiness startup }` | ✓ |
| `init "name" {}` | ✓ |
| Plus: `host`, `tls`, `location`, `lb` | ingress side only |

If you find yourself reaching for a standalone `deployment +
ingress` pair, you can almost always collapse it into an `app`
block. The two split shapes only earn their keep when:

  - You need TWO ingresses pointing at the same deployment (e.g.
    public + admin hosts) — declare the deployment once, two
    `ingress` blocks separately.
  - The service name pointer needs to differ from the
    deployment identity. Rare.

For 90% of web apps, `app` is the right shape.

## Examples

| file | what it shows |
|---|---|
| [`everything.hcl`](./everything.hcl) | Full production stack: postgres + redis statefulsets with probes, registry block, app with init / probes / autoscale / on_deploy (asset-backed PagerDuty body) / resources, plus ingress with TLS. Every shipped milestone wired up in one file. |
| `webhooks/pagerduty-failure.json` | PagerDuty Events API v2 body template referenced from `everything.hcl` via the `asset` block. Shows the recommended asset-backed pattern for non-trivial webhook receivers. |

## Why one big example instead of many small ones

The other example dirs (`probes/`, `init/`, `autoscale/`,
`on_deploy/`, `registry/`) cover each feature in isolation —
that's where you learn the surface of one thing at a time.

This dir's job is the OPPOSITE: show how the features compose
in a single realistic deployment. The `everything.hcl` reads
as "what does a production-grade voodu manifest look like in
2026?" so an operator copy-pasting it gets a coherent stack
they can adapt, not seven separate snippets they have to glue.

If you want to learn one feature at a time, start in the per-
feature dirs. If you want to see how they all sit together,
read [`everything.hcl`](./everything.hcl) end-to-end.
