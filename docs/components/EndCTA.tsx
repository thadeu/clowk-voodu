import InstallShell from './InstallShell';

export default function EndCTA() {
  return (
    <section className="border-t border-voodu-line py-24 text-center">
      <div className="max-w-[1180px] mx-auto px-5 sm:px-8 md:px-10 lg:px-14">
        <h2 className="font-sans font-semibold text-[clamp(34px,5.5vw,64px)] tracking-[-0.03em] leading-none mb-6 text-balance text-white">
          Stop deploying from a Slack thread.
        </h2>
        <div className="inline-flex gap-3 flex-wrap justify-center">
          <InstallShell />
          <a
            href="https://github.com/thadeu/clowk-voodu"
            target="_blank"
            rel="noreferrer"
            className="inline-flex items-center gap-2 px-4 py-3 rounded-[10px] bg-mint-400 border border-mint-400 text-[#07140d] font-mono text-[13px] font-semibold whitespace-nowrap hover:brightness-110 transition-all"
          >
            ★ Star on GitHub
          </a>
        </div>
        <p className="mt-4.5 text-voodu-fg-mute font-mono text-[12px]">
          MIT · v0.9.2 · made by{' '}
          <a
            href="https://github.com/thadeu"
            target="_blank"
            rel="noreferrer"
            className="text-voodu-fg-dim hover:text-mint-400 transition-colors"
          >
            @thadeu
          </a>
        </p>
      </div>
    </section>
  );
}
