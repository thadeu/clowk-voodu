import type { ReactNode } from 'react';
import Link from 'next/link';

import InstallShell from './InstallShell';

type Line =
  | { kind: 'cmd'; text: string }
  | { kind: 'ok'; text: string }
  | { kind: 'final'; text: string }
  | { kind: 'out'; text: string }
  | { kind: 'dim'; text: string }
  | { kind: 'blank' };

type Pillar = {
  tag: string;
  title: string;
  body: ReactNode;
  lines: Line[];
};

const pillars: Pillar[] = [
  {
    tag: 'provision',
    title: 'Apply, and it exists.',
    body: (
      <>
        Postgres replicas, Redis Sentinel, app behind TLS — every kind shipped by one <code>apply</code>. No portals,
        no chart-of-charts.
      </>
    ),
    lines: [
      { kind: 'cmd', text: 'vd apply -f stack.hcl' },
      { kind: 'ok', text: 'packing web' },
      { kind: 'ok', text: 'building release (12s)' },
      { kind: 'ok', text: 'postgres/myorg/db    applied' },
      { kind: 'ok', text: 'redis/myorg/cache    applied' },
      { kind: 'ok', text: 'deployment/myorg/web applied' },
      { kind: 'final', text: 'apply complete (3 resources)' },
    ],
  },
  {
    tag: 'observe',
    title: 'See it. Get notified.',
    body: (
      <>
        Live CPU + memory via <code>vd stats</code>. Per-probe push notifications via <code>on_probe</code> — Telegram,
        Slack, PagerDuty fire the moment a pod transitions. No agent, no Grafana, no portal in the middle.
      </>
    ),
    lines: [
      { kind: 'cmd', text: 'vd stats myorg' },
      { kind: 'dim', text: 'NAME            CPU    MEMORY        REPLICAS' },
      { kind: 'out', text: 'myorg/web       45m    128Mi/512Mi   3/3' },
      { kind: 'out', text: 'myorg/db        180m   512Mi/1Gi     3/3' },
      { kind: 'out', text: 'myorg/cache     12m    64Mi/256Mi    3/3' },
      { kind: 'blank' },
      { kind: 'dim', text: '# on_probe → Telegram on transition' },
      { kind: 'dim', text: '🚨 myorg-db-2 liveness failed: HTTP 503' },
    ],
  },
  {
    tag: 'operate',
    title: 'One CLI, every day.',
    body: (
      <>
        Stream logs across replicas, rollback by release ID, exec into any container. <code>vd</code> is the only
        surface — no SSH gymnastics, no portal hand-offs.
      </>
    ),
    lines: [
      { kind: 'cmd', text: 'vd logs -f myorg/db --tail 2' },
      { kind: 'dim', text: '[14:23:01] replica sync ok' },
      { kind: 'dim', text: '[14:23:02] checkpoint complete' },
      { kind: 'blank' },
      { kind: 'cmd', text: 'vd rollback myorg/web --to v17' },
      { kind: 'ok', text: 'web rolled back to v17 (3 replicas)' },
    ],
  },
];

export default function Lifecycle() {
  return (
    <section id="lifecycle" className="py-24 border-t border-voodu-line">
      <div className="max-w-[1180px] mx-auto px-5 sm:px-8 md:px-10 lg:px-14">
        <div className="font-mono text-[12px] tracking-[0.08em] uppercase text-mint-400 mb-3.5">
          {'// lifecycle'}
        </div>
        <h2 className="font-sans font-semibold text-[clamp(28px,4vw,44px)] tracking-[-0.025em] leading-[1.05] mb-4 text-balance text-white">
          Declared. Observed. Operated.
        </h2>
        <p className="text-voodu-fg-dim max-w-[60ch] text-[17px] mb-10">
          The CLI that ships resources is the CLI that watches them. Postgres replicas, Redis Sentinel state,
          job histories — all visible from one place, without a second portal.
        </p>

        <div className="grid grid-cols-1 lg:grid-cols-3 gap-4">
          {pillars.map((p, i) => (
            <PillarCard key={i} pillar={p} />
          ))}
        </div>

        <div className="mt-10 flex flex-wrap items-center gap-3">
          <InstallShell />
          <Link
            href="/docs/examples/production-stack"
            className="inline-flex items-center gap-2 px-4 py-3 rounded-[10px] border border-voodu-line text-voodu-fg font-mono text-[13px] whitespace-nowrap hover:border-voodu-line-strong hover:text-mint-400 transition-colors"
          >
            See a production stack →
          </Link>
        </div>
      </div>
    </section>
  );
}

function PillarCard({ pillar }: { pillar: Pillar }) {
  return (
    <div className="border border-voodu-line rounded-2xl bg-white/[0.01] overflow-hidden flex flex-col">
      <div className="px-5 pt-5">
        <div className="font-mono text-[11px] text-mint-400 tracking-[0.08em] uppercase mb-2">{pillar.tag}</div>
        <h3 className="m-0 mb-2 font-sans font-semibold text-[20px] tracking-[-0.01em] text-white">{pillar.title}</h3>
      </div>

      <div className="bg-voodu-code mx-5 mt-2 rounded-xl border border-voodu-line font-mono text-[12.5px] leading-[1.65] px-4 py-3.5 overflow-x-auto">
        {pillar.lines.map((l, i) => (
          <LineRow key={i} line={l} />
        ))}
      </div>

      <div className="px-5 pt-3 pb-5 flex-1">
        <p className="m-0 text-voodu-fg-dim text-[13.5px] leading-[1.55] [&_code]:font-mono [&_code]:text-voodu-fg [&_code]:bg-voodu-bg-elev [&_code]:px-1 [&_code]:py-px [&_code]:rounded [&_code]:text-[12.5px]">
          {pillar.body}
        </p>
      </div>
    </div>
  );
}

function LineRow({ line }: { line: Line }) {
  if (line.kind === 'blank') {
    return <div>&nbsp;</div>;
  }

  if (line.kind === 'cmd') {
    return (
      <div className="text-voodu-fg whitespace-pre">
        <span className="text-mint-400 mr-2 select-none">$</span>
        {line.text}
      </div>
    );
  }

  if (line.kind === 'ok') {
    return (
      <div className="text-voodu-fg whitespace-pre">
        <span className="text-mint-400 mr-2 select-none">✓</span>
        {line.text}
      </div>
    );
  }

  if (line.kind === 'final') {
    return (
      <div className="whitespace-pre" style={{ color: '#c7f5dd' }}>
        <span className="mr-2 select-none">✓</span>
        {line.text}
      </div>
    );
  }

  if (line.kind === 'dim') {
    return <div className="text-voodu-fg-mute whitespace-pre">{line.text}</div>;
  }

  return <div className="text-voodu-fg whitespace-pre">{line.text}</div>;
}
