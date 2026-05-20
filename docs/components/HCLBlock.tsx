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
    tag: 'Out of the box',
    h: 'TLS, health, ingress — no plugins',
    body: (
      <>
        Add <code>tls{'{}'}</code> and Voodu cuts a Let&apos;s Encrypt cert via Caddy. Add <code>host</code> and routing
        reconciles automatically. No Helm, no sidecars, no YAML blast radius.
      </>
    ),
  },
  {
    tag: 'Stateful, first-class',
    h: 'Postgres, Mongo, Redis declared like apps',
    body: (
      <>
        HA Redis with a sentinel cluster is six lines. Postgres and Mongo plugins ship backup, replicas, and
        test-restore — the parts every PaaS leaves to you.
      </>
    ),
  },
  {
    tag: 'Upsert-only by default',
    h: 'No accidental deletions',
    body: (
      <>
        <code>apply</code> only creates and updates. Resources missing from the file stay put. Want to clean up?{' '}
        <code>--prune</code> is the explicit opt-in, scoped per <code>(scope, kind)</code> so siblings of other types
        aren&apos;t touched.
      </>
    ),
  },
];

export default function HCLBlock() {
  return (
    <section id="hcl" className="py-24 border-t border-voodu-line">
      <div className="max-w-[1180px] mx-auto px-5 sm:px-8 md:px-10 lg:px-14">
        <div className="font-mono text-[12px] tracking-[0.08em] uppercase text-mint-400 mb-3.5">// the manifest</div>
        <h2 className="font-sans font-semibold text-[clamp(28px,4vw,44px)] tracking-[-0.025em] leading-[1.05] mb-4 text-balance text-white">
          One file describes the running system.
        </h2>
        <p className="text-voodu-fg-dim max-w-[60ch] text-[17px] mb-10">
          HCL out-of-the-box. No YAML to coerce, no Compose tricks, no chart-of-charts. Apps, ingress, and stateful
          services share the same shape — and the same blast radius.
        </p>

        <div className="grid grid-cols-1 lg:grid-cols-[minmax(0,1.2fr)_minmax(0,1fr)] gap-8 items-stretch">
          <div className="bg-voodu-code border border-voodu-line-strong rounded-2xl overflow-hidden font-mono text-[13px] leading-[1.65]">
            <div className="flex items-center justify-between gap-3 px-3.5 py-2.5 border-b border-voodu-line text-voodu-fg-mute text-[11px] font-mono whitespace-nowrap">
              <span className="text-voodu-fg whitespace-nowrap">voodu.hcl</span>
              <span className="whitespace-nowrap">HCL · 22 lines</span>
            </div>
            <pre className="m-0 px-5 py-4.5 text-voodu-fg overflow-x-auto whitespace-pre">
              <code>
                <span className="text-tk-com italic">
                  {`// Ship a web app with TLS + 3 replicas behind a load balancer.\n`}
                </span>
                <span className="text-tk-block">app</span> <span className="text-tk-str">&quot;myapp&quot;</span>{' '}
                <span className="text-tk-str">&quot;web&quot;</span> <span className="text-tk-pun">{'{\n'}</span>
                {'  '}
                <span className="text-tk-key">image</span>
                {'    = '}
                <span className="text-tk-str">&quot;ghcr.io/myorg/myapp:latest&quot;</span>
                {'\n'}
                {'  '}
                <span className="text-tk-key">replicas</span>
                {' = '}
                <span className="text-tk-num">3</span>
                {'\n\n'}
                {'  '}
                <span className="text-tk-key">env</span>
                {' = '}
                <span className="text-tk-pun">{'{\n'}</span>
                {'    '}
                <span className="text-tk-key">PORT</span>
                {'     = '}
                <span className="text-tk-str">&quot;8080&quot;</span>
                {'\n'}
                {'    '}
                <span className="text-tk-key">NODE_ENV</span>
                {' = '}
                <span className="text-tk-str">&quot;production&quot;</span>
                {'\n'}
                {'  '}
                <span className="text-tk-pun">{'}\n\n'}</span>
                {'  '}
                <span className="text-tk-key">health_check</span>
                {' = '}
                <span className="text-tk-str">&quot;/healthz&quot;</span>
                {'\n'}
                {'  '}
                <span className="text-tk-key">host</span>
                {'         = '}
                <span className="text-tk-str">&quot;myapp.example.com&quot;</span>
                {'\n\n'}
                {'  '}
                <span className="text-tk-block">tls</span> <span className="text-tk-pun">{'{\n'}</span>
                {'    '}
                <span className="text-tk-key">email</span>
                {' = '}
                <span className="text-tk-str">&quot;ops@example.com&quot;</span>
                {'\n'}
                {'  '}
                <span className="text-tk-pun">{'}\n'}</span>
                <span className="text-tk-pun">{'}\n\n'}</span>
                <span className="text-tk-com italic">
                  {`// HA Redis: primary + sentinel cluster, declared, not orchestrated.\n`}
                </span>
                <span className="text-tk-block">redis</span> <span className="text-tk-str">&quot;clowk-lp&quot;</span>{' '}
                <span className="text-tk-str">&quot;redis&quot;</span> <span className="text-tk-pun">{'{\n'}</span>
                {'  '}
                <span className="text-tk-key">replicas</span>
                {' = '}
                <span className="text-tk-num">3</span>
                {'\n'}
                <span className="text-tk-pun">{'}\n\n'}</span>
                <span className="text-tk-block">redis</span> <span className="text-tk-str">&quot;clowk-lp&quot;</span>{' '}
                <span className="text-tk-str">&quot;redis-ha&quot;</span> <span className="text-tk-pun">{'{\n'}</span>
                {'  '}
                <span className="text-tk-block">sentinel</span> <span className="text-tk-pun">{'{\n'}</span>
                {'    '}
                <span className="text-tk-key">monitor</span>
                {' = '}
                <span className="text-tk-str">&quot;clowk-lp/redis&quot;</span>
                {'\n'}
                {'  '}
                <span className="text-tk-pun">{'}\n'}</span>
                <span className="text-tk-pun">{'}'}</span>
              </code>
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
