import InstallShell from './InstallShell';
import HeroTerminal from './HeroTerminal';

// Resolved at build time by docs.yml from the latest GitHub release tag.
// Fallback to "dev" for local pnpm dev (no CI context).
const VOODU_VERSION = process.env.NEXT_PUBLIC_VOODU_VERSION ?? 'dev';

export default function Hero() {
  return (
    <header
      id="top"
      className="relative max-w-[1180px] mx-auto px-5 sm:px-8 md:px-10 lg:px-14 pt-[88px] sm:pt-[104px] pb-10"
    >
      <div className="grid grid-cols-1 lg:grid-cols-[minmax(0,1.05fr)_minmax(0,0.95fr)] gap-12 items-end">
        <div>
          <span className="inline-flex items-center gap-2 font-mono text-[12px] tracking-[0.04em] text-voodu-fg-dim mb-6 py-1 pl-1.5 pr-2.5 border border-voodu-line rounded-full whitespace-nowrap">
            <img src="icons/loading-64.gif" alt="Voodu icon" className="w-4 h-4" />
            {VOODU_VERSION} · <code className="font-mono text-voodu-fg ml-2">voodu apply</code>, no git push
          </span>

          <h1 className="font-sans font-semibold text-[clamp(40px,7vw,88px)] leading-[0.98] tracking-[-0.035em] mb-6 text-balance text-white">
            Self-hosted PaaS.
            <br />
            <span className="text-mint-400">Commitless</span> deploys.
          </h1>

          <p className="text-[clamp(16px,1.5vw,19px)] text-voodu-fg-dim max-w-[56ch] mb-8 leading-[1.55]">
            Voodu is a Heroku-shaped, Kubernetes-honest deploy tool you run on your own boxes. One HCL file. One{' '}
            <code className="font-mono text-[0.95em]">voodu apply</code>. No git push, no bare repo, no plugin sprawl —
            apps, ingress with TLS, and stateful services that actually back themselves up.
          </p>

          <div className="flex flex-wrap gap-3 items-center">
            <InstallShell />
            <a
              href="https://github.com/thadeu/clowk-voodu"
              target="_blank"
              rel="noreferrer"
              className="inline-flex items-center gap-2 px-4 py-3 rounded-[10px] border border-mint-400 bg-mint-400 text-[#07140d] font-mono text-[13px] font-semibold whitespace-nowrap transition-all hover:brightness-110"
            >
              ★ Star on GitHub
            </a>
            <a
              href="/docs"
              className="inline-flex items-center gap-2 px-4 py-3 rounded-[10px] border border-voodu-line-strong font-mono text-[13px] text-voodu-fg whitespace-nowrap transition-all hover:bg-voodu-bg-elev hover:border-mint-400"
            >
              Read the docs ↗
            </a>
          </div>

          <div className="mt-5 flex flex-wrap gap-2">
            <Pill>MIT licensed</Pill>
            <Pill>Linux · macOS</Pill>
            <Pill>Go · Caddy · Docker</Pill>
            <Pill accent>100% self-hosted</Pill>
          </div>
        </div>

        <div>
          <HeroTerminal />
        </div>
      </div>
    </header>
  );
}

function Pill({ children, accent = false }: { children: React.ReactNode; accent?: boolean }) {
  return (
    <span
      className={`inline-flex items-center gap-1.5 px-2 py-0.5 rounded-full font-mono text-[11px] whitespace-nowrap border ${
        accent ? 'text-mint-400 border-mint-400/50 bg-mint-400/10' : 'text-voodu-fg-dim border-voodu-line'
      }`}
    >
      {children}
    </span>
  );
}
