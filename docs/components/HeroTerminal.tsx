'use client';

import { useEffect, useMemo, useState } from 'react';

type Line =
  | { kind: 'cmd'; text: string }
  | { kind: 'out'; text: string }
  | { kind: 'ok'; text: string }
  | { kind: 'tag'; prefix: string; color: 'add' | 'warn' | 'rm'; text: string };

const COLOR_CLASS: Record<'add' | 'warn' | 'rm', string> = {
  add: 'text-mint-400',
  warn: 'text-[#ffb454]',
  rm: 'text-[#ff7b7b]',
};

export default function HeroTerminal() {
  const lines = useMemo<Line[]>(
    () => [
      { kind: 'cmd', text: 'voodu apply -f voodu.hcl -r prod-1' },
      { kind: 'out', text: '→ packing context (1.4 MB)' },
      { kind: 'out', text: '→ streaming over ssh ubuntu@prod-1' },
      { kind: 'out', text: '→ controller: planning ...' },
      {
        kind: 'tag',
        prefix: '+',
        color: 'add',
        text: ' deployment/prod/api      replicas=3 image=ghcr.io/myorg/api:1.7',
      },
      { kind: 'tag', prefix: '~', color: 'warn', text: ' ingress/prod/api         tls.email=ops@example.com' },
      { kind: 'tag', prefix: '+', color: 'add', text: ' redis/clowk-lp/redis-ha  sentinel: monitor=clowk-lp/redis' },
      { kind: 'out', text: '→ build → swap current → reconcile caddy' },
      { kind: 'ok', text: '✓ apply complete in 11.8s' },
      { kind: 'out', text: '✓ https://api.example.com  ·  3/3 healthy' },
    ],
    []
  );

  const [shown, setShown] = useState(0);

  useEffect(() => {
    if (shown >= lines.length) return;

    const t = setTimeout(() => setShown(s => s + 1), shown === 0 ? 700 : 380);

    return () => clearTimeout(t);
  }, [shown, lines.length]);

  return (
    <div
      aria-hidden="true"
      className="bg-voodu-code border border-voodu-line-strong rounded-2xl overflow-hidden shadow-[0_30px_60px_-30px_rgba(0,0,0,0.6)] font-mono text-[12.5px] leading-[1.7]"
    >
      <div className="flex items-center gap-1.5 px-3 py-2.5 border-b border-voodu-line text-voodu-fg-mute text-[11px]">
        <div className="flex gap-1.5 mr-2">
          <span className="w-2.5 h-2.5 rounded-full bg-voodu-line-strong" />
          <span className="w-2.5 h-2.5 rounded-full bg-voodu-line-strong" />
          <span className="w-2.5 h-2.5 rounded-full bg-voodu-line-strong" />
        </div>
        <span>~/myorg/api · zsh</span>
      </div>
      <div className="px-4 py-4 min-h-[320px] voodu-typein">
        {lines.slice(0, shown).map((l, i) => {
          if (l.kind === 'cmd') {
            return (
              <span key={i} className="block text-voodu-fg">
                <span className="text-mint-400 mr-1.5">$</span>
                {l.text}
              </span>
            );
          }

          if (l.kind === 'out') {
            return (
              <span key={i} className="block text-voodu-fg-dim">
                {l.text}
              </span>
            );
          }

          if (l.kind === 'ok') {
            return (
              <span key={i} className="block text-mint-400">
                {l.text}
              </span>
            );
          }

          return (
            <span key={i} className={`block ${COLOR_CLASS[l.color]}`}>
              <span className="inline-block w-3.5">{l.prefix}</span>
              {l.text}
            </span>
          );
        })}
        {shown >= lines.length && (
          <span className="block text-voodu-fg">
            <span className="text-mint-400 mr-1.5">$</span>
            <span className="voodu-caret" />
          </span>
        )}
      </div>
    </div>
  );
}
