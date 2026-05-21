'use client';

import { Fragment, useState } from 'react';
import type { ReactNode } from 'react';

const annotations: { tag: string; h: string; body: ReactNode }[] = [
  {
    tag: 'Two labels',
    h: 'scope + name, every kind',
    body: (
      <>
        Every resource is keyed by <code>(scope, name)</code>. Scope groups apps, envs or teams; name is unique inside
        it. The same key flows through <code>diff</code>, <code>apply</code>, and prune.
      </>
    ),
  },
  {
    tag: "Provision, don't orchestrate",
    h: 'Databases ship as first-class kinds',
    body: (
      <>
        Postgres with replicas + nightly backup, Redis with Sentinel — declared in the same file as your app. Mongo,
        RabbitMQ, Kafka land via the plugin model; <code>/opt/voodu/plugins</code> is where new kinds appear.
      </>
    ),
  },
  {
    tag: 'Out of the box',
    h: 'TLS, health, ingress — no chart',
    body: (
      <>
        Add <code>tls{'{}'}</code> and voodu cuts a Let&apos;s Encrypt cert via Caddy. <code>host</code> wires ingress.
        No Helm, no sidecars, no YAML blast radius.
      </>
    ),
  },
  {
    tag: 'Upsert-only by default',
    h: 'No accidental deletions',
    body: (
      <>
        <code>apply</code> only creates and updates. Resources missing from the file stay put. <code>--prune</code>{' '}
        is the explicit opt-in, scoped per <code>(scope, kind)</code> so siblings of other types aren&apos;t touched.
      </>
    ),
  },
];

type Tab = { id: string; label: string; code: string };

const tabs: Tab[] = [
  {
    id: 'ha',
    label: 'HA stack',
    code: `// Production stack: HA postgres + HA redis behind the app.
app "myorg" "web" {
  image    = "ghcr.io/myorg/web:latest"
  replicas = 3
  host     = "app.example.com"

  env = {
    DATABASE_URL = "postgres://myorg/db"
    REDIS_URL    = "redis://myorg/cache"
  }
  tls { email = "ops@example.com" }
}

postgres "myorg" "db" {
  image    = "postgres:16"
  replicas = 3            // 1 primary + 2 standbys
  database = "myapp"
}

redis "myorg" "cache" {
  replicas = 3
}

redis "myorg" "cache-ha" {
  sentinel { monitor = "myorg/cache" }
}`,
  },
  {
    id: 'db',
    label: 'App + Postgres',
    code: `// Web app backed by managed Postgres with streaming replication.
app "myorg" "web" {
  image    = "ghcr.io/myorg/web:latest"
  replicas = 3
  host     = "app.example.com"

  env = {
    DATABASE_URL = "postgres://myorg/db"
  }
  tls { email = "ops@example.com" }
}

postgres "myorg" "db" {
  image    = "postgres:16"
  replicas = 2            // 1 primary + 1 standby
  database = "myapp"

  pg_config = {
    max_connections = 200
    shared_buffers  = "1GB"
  }
}`,
  },
  {
    id: 'web',
    label: 'Web app',
    code: `// Ship a web app with TLS + 3 replicas behind a load balancer.
app "myorg" "web" {
  image    = "ghcr.io/myorg/web:latest"
  replicas = 3
  host     = "app.example.com"

  env = {
    PORT     = "8080"
    NODE_ENV = "production"
  }

  health_check = "/healthz"
  tls { email = "ops@example.com" }
}`,
  },
  {
    id: 'jobs',
    label: 'Jobs',
    code: `// On-deploy migrations + nightly Postgres backup to S3.
// Backups live as cronjobs, not a magic block on the postgres resource.
job "myorg" "migrate" {
  image    = "ghcr.io/myorg/web:latest"
  command  = ["bin/rails", "db:migrate"]
  env_from = ["myorg/shared"]
  timeout  = "5m"
}

cronjob "myorg" "pg-backup" {
  schedule = "0 3 * * *"
  timezone = "America/Sao_Paulo"

  image    = "amazon/aws-cli:latest"
  command  = ["s3", "sync", "/backups/", "s3://my-bucket/myapp/"]

  env_from = ["myorg/shared"]
}`,
  },
];

export default function HCLBlock() {
  const [activeId, setActiveId] = useState(tabs[0].id);

  const active = tabs.find(t => t.id === activeId) ?? tabs[0];
  const lineCount = active.code.split('\n').length;

  return (
    <section id="hcl" className="py-24 border-t border-voodu-line">
      <div className="max-w-[1180px] mx-auto px-5 sm:px-8 md:px-10 lg:px-14">
        <div className="font-mono text-[12px] tracking-[0.08em] uppercase text-mint-400 mb-3.5">// the manifest</div>
        <h2 className="font-sans font-semibold text-[clamp(28px,4vw,44px)] tracking-[-0.025em] leading-[1.05] mb-4 text-balance text-white">
          One file describes the running system.
        </h2>
        <p className="text-voodu-fg-dim max-w-[60ch] text-[17px] mb-10">
          HCL out-of-the-box. No YAML to coerce, no Compose tricks, no chart-of-charts. Apps, ingress, databases,
          jobs — every kind shares the same shape and the same blast radius.
        </p>

        <div className="grid grid-cols-1 lg:grid-cols-[minmax(0,1.2fr)_minmax(0,1fr)] gap-8 items-stretch">
          <div className="bg-voodu-code border border-voodu-line-strong rounded-2xl overflow-hidden font-mono text-[13px] leading-[1.65] flex flex-col">
            <div
              role="tablist"
              aria-label="Manifest shapes"
              className="flex items-center gap-1 px-2 pt-2 border-b border-voodu-line overflow-x-auto"
            >
              {tabs.map(t => {
                const isActive = t.id === activeId;

                return (
                  <button
                    key={t.id}
                    role="tab"
                    aria-selected={isActive}
                    onClick={() => setActiveId(t.id)}
                    className={
                      'px-3 py-1.5 rounded-t-lg text-[12px] font-mono tracking-[0.02em] whitespace-nowrap transition-colors ' +
                      (isActive
                        ? 'text-mint-400 bg-voodu-bg-elev border border-voodu-line border-b-voodu-code -mb-px'
                        : 'text-voodu-fg-mute hover:text-voodu-fg')
                    }
                  >
                    {t.label}
                  </button>
                );
              })}

              <span className="ml-auto pr-2 text-voodu-fg-mute text-[11px] whitespace-nowrap">
                voodu.hcl · HCL · {lineCount} lines
              </span>
            </div>

            <pre className="m-0 px-5 py-4.5 text-voodu-fg overflow-x-auto whitespace-pre">
              <code>{highlightHCL(active.code)}</code>
            </pre>
          </div>

          <div className="flex flex-col gap-3.5">
            {annotations.map((a, i) => (
              <div key={i} className="border border-voodu-line bg-white/[0.01] rounded-xl px-5 py-4.5">
                <div className="font-mono text-[11px] text-mint-400 tracking-[0.06em] uppercase mb-1.5">{a.tag}</div>
                <h4 className="m-0 mb-1.5 font-sans font-semibold text-[17px] tracking-[-0.01em] text-white">{a.h}</h4>
                <p className="m-0 text-voodu-fg-dim text-[14px] [&_code]:font-mono [&_code]:text-voodu-fg [&_code]:bg-voodu-bg-elev [&_code]:px-1.5 [&_code]:py-px [&_code]:rounded [&_code]:text-[12.5px]">
                  {a.body}
                </p>
              </div>
            ))}
          </div>
        </div>
      </div>
    </section>
  );
}

function highlightHCL(code: string): ReactNode {
  const lines = code.split('\n');

  return lines.map((line, i) => (
    <Fragment key={i}>
      {renderLine(line)}
      {i < lines.length - 1 ? '\n' : ''}
    </Fragment>
  ));
}

function renderLine(line: string): ReactNode {
  const trimmed = line.trimStart();
  const indent = line.slice(0, line.length - trimmed.length);

  if (trimmed.startsWith('//')) {
    return (
      <>
        {indent}
        <span className="text-tk-com italic">{trimmed}</span>
      </>
    );
  }

  if (trimmed === '') return line;

  const commentIdx = findInlineCommentIdx(trimmed);

  if (commentIdx >= 0) {
    const codePart = trimmed.slice(0, commentIdx);
    const commentPart = trimmed.slice(commentIdx);

    return (
      <>
        {indent}
        {tokenize(codePart)}
        <span className="text-tk-com italic">{commentPart}</span>
      </>
    );
  }

  return (
    <>
      {indent}
      {tokenize(trimmed)}
    </>
  );
}

function findInlineCommentIdx(s: string): number {
  let inString = false;

  for (let i = 0; i < s.length - 1; i++) {
    if (s[i] === '"') {
      inString = !inString;
      continue;
    }

    if (!inString && s[i] === '/' && s[i + 1] === '/') return i;
  }

  return -1;
}

function tokenize(content: string): ReactNode[] {
  const tokens: ReactNode[] = [];
  const firstIdentRole = classifyFirstIdent(content);
  let firstIdentConsumed = false;
  let key = 0;

  const re = /\s+|"[^"]*"|\d+(?:\.\d+)?|[{}[\],=]|[A-Za-z_][A-Za-z0-9_]*|./g;
  let match: RegExpExecArray | null;

  while ((match = re.exec(content)) !== null) {
    const tok = match[0];

    if (/^\s+$/.test(tok)) {
      tokens.push(tok);
      continue;
    }

    if (/^"[^"]*"$/.test(tok)) {
      tokens.push(
        <span key={key++} className="text-tk-str">
          {tok}
        </span>
      );
      continue;
    }

    if (/^\d/.test(tok)) {
      tokens.push(
        <span key={key++} className="text-tk-num">
          {tok}
        </span>
      );
      continue;
    }

    if (/^[{}[\],=]$/.test(tok)) {
      tokens.push(
        <span key={key++} className="text-tk-pun">
          {tok}
        </span>
      );
      continue;
    }

    if (/^[A-Za-z_]/.test(tok)) {
      if (!firstIdentConsumed) {
        firstIdentConsumed = true;
        const cls = firstIdentRole === 'block' ? 'text-tk-block' : 'text-tk-key';

        tokens.push(
          <span key={key++} className={cls}>
            {tok}
          </span>
        );

        continue;
      }
    }

    tokens.push(tok);
  }

  return tokens;
}

function classifyFirstIdent(content: string): 'block' | 'key' {
  const m = content.match(/^[a-z_][a-z0-9_]*/);

  if (!m) return 'key';

  const after = content.slice(m[0].length).trimStart();

  if (after.startsWith('"') || after.startsWith('{')) return 'block';

  return 'key';
}
