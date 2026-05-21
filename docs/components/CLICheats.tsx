const items = [
  {
    label: 'Bootstrap a host',
    cmd: 'voodu remote setup prod-1 ubuntu@host',
    desc: 'SSH preflight + install + server-mode setup. Idempotent.',
  },
  {
    label: 'Create an app',
    cmd: 'voodu apps create prod',
    desc: 'Seeds /opt/voodu/apps/prod with a fresh .env.',
  },
  {
    label: 'Apply a manifest',
    cmd: 'voodu apply -f voodu -r prod-1',
    desc: 'Streams context over SSH, reconciles, swaps containers.',
  },
  {
    label: 'Preview drift',
    cmd: 'voodu diff -f voodu.hcl --detailed-exitcode',
    desc: 'Plan-style output. Exit 2 means changes pending.',
  },
  {
    label: 'Manage env',
    cmd: 'voodu config set DATABASE_URL=… -a prod',
    desc: 'Out-of-band secrets. Always wins over manifest env blocks.',
  },
  {
    label: 'Install a plugin',
    cmd: 'voodu plugins:install thadeu/voodu-postgres',
    desc: 'Drops a binary into /opt/voodu/plugins. Voodu wires it up.',
  },
];

export default function CLICheats() {
  return (
    <section className="py-24 border-t border-voodu-line">
      <div className="max-w-[1180px] mx-auto px-5 sm:px-8 md:px-10 lg:px-14">
        <div className="font-mono text-[12px] tracking-[0.08em] uppercase text-mint-400 mb-3.5">// cli</div>
        <h2 className="font-sans font-semibold text-[clamp(28px,4vw,44px)] tracking-[-0.025em] leading-[1.05] mb-4 text-balance text-white">
          A small, k8s-shaped command surface.
        </h2>
        <p className="text-voodu-fg-dim max-w-[60ch] text-[17px] mb-10">
          Six verbs cover ~95% of day-to-day. Everything else is a plugin discovered from{' '}
          <code className="font-mono text-voodu-fg bg-voodu-bg-elev px-1.5 py-px rounded text-[14px]">
            /opt/voodu/plugins
          </code>
          .
        </p>

        <div className="grid grid-cols-1 sm:grid-cols-2 border border-voodu-line rounded-2xl overflow-hidden bg-white/[0.01]">
          {items.map((it, i) => (
            <div
              key={i}
              className="px-6 py-5 border-r border-b border-voodu-line last:border-b-0 sm:[&:nth-child(2n)]:border-r-0 sm:[&:nth-last-child(-n+2)]:border-b-0 max-sm:border-r-0 max-sm:[&:nth-last-child(2)]:border-b-[1px]"
            >
              <div className="font-mono text-[11px] text-voodu-fg-mute tracking-[0.06em] uppercase mb-2">
                {it.label}
              </div>
              <code className="block font-mono text-[14px] text-voodu-fg mb-1.5">
                <span className="text-mint-400 mr-2">$</span>
                {it.cmd}
              </code>
              <p className="m-0 text-voodu-fg-dim text-[13.5px]">{it.desc}</p>
            </div>
          ))}
        </div>
      </div>
    </section>
  );
}
