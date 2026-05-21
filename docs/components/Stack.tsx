const built = [
  { tag: 'core', h: 'Go controller', p: 'Single static binary, embedded etcd, HTTP API on 127.0.0.1:8686.' },
  { tag: 'core', h: 'Caddy ingress', p: 'Replaces NGINX. Caddy Admin API, ACME, on-demand wildcard TLS.' },
  { tag: 'core', h: 'Docker builds', p: 'Bring a Dockerfile or let voodu auto-detect Go, Ruby, Rails, Python, Node.' },
  { tag: 'core', h: 'SSH transport', p: 'Tarball over SSH. No daemons listening on the public internet.' },
];

const plugins = [
  { tag: 'plugin', h: 'voodu-postgres', p: 'Postgres with backup, replica and test-restore. Declared like an app.' },
  { tag: 'plugin', h: 'voodu-redis', p: 'Redis service with the same reliability primitives.' },
  { tag: 'plugin', h: 'voodu-caddy', p: 'Reference plugin. Read it to write your own.' },
  {
    tag: 'plugin',
    h: 'Build your own',
    p: 'Independent binaries discovered from /opt/voodu/plugins. Install via GitHub repo.',
  },
];

export default function Stack() {
  return (
    <section id="stack" className="py-24 border-t border-voodu-line">
      <div className="max-w-[1180px] mx-auto px-5 sm:px-8 md:px-10 lg:px-14">
        <div className="font-mono text-[12px] tracking-[0.08em] uppercase text-mint-400 mb-3.5">// stack</div>
        <h2 className="font-sans font-semibold text-[clamp(28px,4vw,44px)] tracking-[-0.025em] leading-[1.05] mb-4 text-balance text-white">
          Built on boring, well-loved parts.
        </h2>
        <p className="text-voodu-fg-dim max-w-[60ch] text-[17px] mb-10">
          Voodu is opinionated where it counts and pluggable where you&apos;d want it to be. Postgres, Mongo, Redis,
          ingress — independent binaries, not a rewritten universe.
        </p>

        <div className="grid gap-6">
          <Group label="core" items={built} />
          <Group label="plugins" items={plugins} />
        </div>
      </div>
    </section>
  );
}

function Group({ label, items }: { label: string; items: { tag: string; h: string; p: string }[] }) {
  return (
    <div>
      <div className="font-mono text-[12px] tracking-[0.08em] uppercase text-mint-400 mb-3">{label}</div>
      <div className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-4 gap-3">
        {items.map((c, i) => (
          <div
            key={i}
            className="border border-voodu-line rounded-xl px-5 py-4.5 bg-white/[0.01] transition-all hover:border-voodu-line-strong"
          >
            <div className="font-mono text-[10.5px] text-mint-400 tracking-[0.08em] uppercase mb-2">{c.tag}</div>
            <h4 className="m-0 mb-1.5 font-sans font-semibold text-[17px] tracking-[-0.01em] text-white">{c.h}</h4>
            <p className="m-0 text-voodu-fg-dim text-[13.5px] leading-[1.5]">{c.p}</p>
          </div>
        ))}
      </div>
    </div>
  );
}
