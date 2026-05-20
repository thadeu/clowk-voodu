import type { ReactNode } from 'react';

const qs: { q: string; a: ReactNode }[] = [
  {
    q: 'Why HCL and not YAML?',
    a: (
      <>
        HCL is built for humans declaring infrastructure. Blocks, labels, and typed values map cleanly to{' '}
        <code>kind/scope/name</code>. Voodu is HCL-only — no YAML coercion, no indentation traps.
      </>
    ),
  },
  {
    q: 'How is this different from Dokku, Coolify, CapRover?',
    a: (
      <>
        Those are excellent — and mostly git-push or click-driven. Voodu is declarative at the layer above: one file
        describes the system, <code>diff</code> shows the plan, <code>apply</code> reconciles. Stateful services are
        first-class, not bolted on as plugins-of-plugins.
      </>
    ),
  },
  {
    q: 'Is it really commitless?',
    a: (
      <>
        Yes. <code>voodu apply</code> tars whatever directory you&apos;re in and streams it over SSH. No bare repo, no{' '}
        <code>git push</code>, no commit required. Edit, save, apply.
      </>
    ),
  },
  {
    q: 'What about K8s?',
    a: (
      <>
        Voodu borrows the shape (declarative, reconciler, source-of-truth) and skips the operations bill (no CNI, no
        Helm chart, no kube-state-metrics dashboard). One controller, embedded etcd, done.
      </>
    ),
  },
  {
    q: 'Does it scale beyond one host?',
    a: (
      <>
        Three prod boxes behind an ALB? Add three remotes and loop{' '}
        <code>voodu apply -f voodu.hcl -r $r</code>. Same manifest, different SSH targets. The controller is per-host
        and stateless about its peers — keep it boring.
      </>
    ),
  },
  {
    q: 'How do secrets work?',
    a: (
      <>
        <code>voodu config set</code> stores env vars per-app, out-of-band from the manifest. <code>config:set</code>{' '}
        always wins over <code>env</code> blocks, so a runaway apply can&apos;t reset a production secret.
      </>
    ),
  },
  {
    q: 'License?',
    a: <>MIT. Self-hosted. No cloud tier, no usage telemetry, no rug pulls planned.</>,
  },
];

export default function FAQ() {
  return (
    <section id="faq" className="py-24 border-t border-voodu-line">
      <div className="max-w-[1180px] mx-auto px-5 sm:px-8 md:px-10 lg:px-14">
        <div className="font-mono text-[12px] tracking-[0.08em] uppercase text-mint-400 mb-3.5">// faq</div>
        <h2 className="font-sans font-semibold text-[clamp(28px,4vw,44px)] tracking-[-0.025em] leading-[1.05] mb-6 text-balance text-white">
          Questions devs actually ask.
        </h2>

        <div className="mt-6 border-t border-voodu-line">
          {qs.map((it, i) => (
            <details
              key={i}
              {...(i === 0 ? { open: true } : {})}
              className="border-b border-voodu-line py-5.5 group [&_code]:font-mono [&_code]:bg-voodu-bg-elev [&_code]:px-1.5 [&_code]:py-px [&_code]:rounded [&_code]:text-voodu-fg [&_code]:text-[13px]"
            >
              <summary className="list-none cursor-pointer flex items-baseline justify-between gap-4 font-sans font-semibold text-[19px] tracking-[-0.015em] text-white [&::-webkit-details-marker]:hidden">
                <span>{it.q}</span>
                <span className="font-mono text-voodu-fg-mute text-[14px] transition-transform duration-200 group-open:rotate-45 group-open:text-mint-400">
                  +
                </span>
              </summary>
              <div className="pt-3.5 text-voodu-fg-dim max-w-[70ch] text-[15px]">{it.a}</div>
            </details>
          ))}
        </div>
      </div>
    </section>
  );
}
