export default function HowItWorks() {
  return (
    <section id="how" className="py-24 border-t border-voodu-line">
      <div className="max-w-[1180px] mx-auto px-5 sm:px-8 md:px-10 lg:px-14">
        <div className="font-mono text-[12px] tracking-[0.08em] uppercase text-mint-400 mb-3.5">// how it works</div>
        <h2 className="font-sans font-semibold text-[clamp(28px,4vw,44px)] tracking-[-0.025em] leading-[1.05] mb-4 text-balance text-white">
          Laptop talks. Server reconciles.
        </h2>
        <p className="text-voodu-fg-dim max-w-[60ch] text-[17px] mb-10">
          The CLI streams your build context over SSH. The controller diffs it against embedded etcd, builds, swaps,
          and reroutes. It&apos;s k8s-shaped reconciliation without the k8s-shaped operations bill.
        </p>

        <pre
          aria-label="Architecture diagram"
          className="m-0 font-mono text-[12.5px] text-voodu-fg-dim bg-voodu-code border border-voodu-line-strong rounded-2xl px-6 py-5 overflow-x-auto leading-[1.6] whitespace-pre"
        >
          {`your laptop                                 your server
───────────                                 ───────────
$ `}
          <span className="text-mint-400">voodu apply -f voodu.hcl</span>
          {`  ──ssh──▶  `}
          <span className="text-voodu-fg">voodu-controller</span>
          {`
  │                                         │
  │                                         └─ reconcile ingress / services (etcd)
  │  (build mode only: stream tarball)
  └─ tar -czf - <path>     ──ssh──▶  `}
          <span className="text-voodu-fg">voodu receive-pack</span>
          {`
                                              └─ extract → build image
                                                 → swap \`current\` symlink
                                                 → run post_deploy hooks
                                                 → recreate container`}
        </pre>

        <div className="grid grid-cols-1 md:grid-cols-3 gap-4 mt-8">
          <Step
            num="01 — DECLARE"
            title="Write the manifest"
            body="One HCL file, or many. Apps, ingresses, databases, jobs — pick what you need, ignore the rest."
            glyph={
              <>
                {`# voodu.hcl\napp "prod" "api" `}
                <span className="text-mint-400">{`{ ... }`}</span>
                {`\ningress "prod" "api" `}
                <span className="text-mint-400">{`{ ... }`}</span>
              </>
            }
          />
          <Step
            num="02 — DIFF"
            title="See the plan first"
            body={
              <>
                <code>voodu diff</code> dry-runs the apply: what&apos;s new, changed, pruned. Wire{' '}
                <code>--detailed-exitcode</code> in CI to gate deploys.
              </>
            }
            glyph={
              <>
                {`$ `}
                <span className="text-mint-400">voodu diff</span>
                {` -f voodu.hcl\n~ deployment/prod/api\n  ~ replicas  1 → 3\n+ redis/clowk-lp/redis-ha`}
              </>
            }
          />
          <Step
            num="03 — APPLY"
            title="Ship over SSH"
            body={
              <>
                Same flags, no surprises. Tarball is content-addressed — identical trees skip the rebuild and just
                repoint <code>current</code>.
              </>
            }
            glyph={
              <>
                {`$ `}
                <span className="text-mint-400">voodu apply</span>
                {` -f voodu.hcl -r prod-1\n✓ apply complete in 11.8s\n✓ 3/3 healthy`}
              </>
            }
          />
        </div>
      </div>
    </section>
  );
}

function Step({
  num,
  title,
  body,
  glyph,
}: {
  num: string;
  title: string;
  body: React.ReactNode;
  glyph: React.ReactNode;
}) {
  return (
    <div className="border border-voodu-line rounded-2xl px-6 py-6 bg-white/[0.01] relative">
      <div className="font-mono text-[11px] text-mint-400 tracking-[0.08em] mb-3.5 flex items-center gap-2 whitespace-nowrap before:content-[''] before:w-2 before:h-2 before:rounded-full before:bg-mint-400">
        {num}
      </div>
      <h3 className="m-0 mb-2.5 font-sans font-semibold text-[19px] tracking-[-0.015em] text-white">{title}</h3>
      <p className="m-0 mb-3.5 text-voodu-fg-dim text-[14.5px] [&_code]:font-mono [&_code]:text-voodu-fg [&_code]:bg-voodu-bg-elev [&_code]:px-1.5 [&_code]:py-px [&_code]:rounded [&_code]:text-[12.5px]">
        {body}
      </p>
      <pre className="m-0 mt-4 font-mono text-[11.5px] text-voodu-fg-mute leading-[1.6] bg-voodu-bg-elev border border-voodu-line rounded-lg px-3 py-2.5 whitespace-pre overflow-x-auto">
        {glyph}
      </pre>
    </div>
  );
}
