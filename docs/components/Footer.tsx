export default function Footer() {
  return (
    <footer className="border-t border-voodu-line py-10 pb-14 text-voodu-fg-mute font-mono text-[12.5px]">
      <div className="max-w-[1180px] mx-auto px-5 sm:px-8 md:px-10 lg:px-14 flex flex-wrap justify-between items-center gap-4">
        <div className="flex items-center gap-2.5">
          <span className="inline-grid place-items-center w-[18px] h-[18px] rounded bg-mint-400 text-[#07140d] font-mono font-extrabold text-[11px] leading-none">
            V
          </span>
          <span>voodu — self-hosted deploys, MIT.</span>
        </div>
        <div className="flex gap-5">
          <a href="https://github.com/thadeu/clowk-voodu" target="_blank" rel="noreferrer" className="hover:text-voodu-fg transition-colors">
            GitHub
          </a>
          <a
            href="https://github.com/thadeu/clowk-voodu/releases"
            target="_blank"
            rel="noreferrer"
            className="hover:text-voodu-fg transition-colors"
          >
            Releases
          </a>
          <a href="/docs" className="hover:text-voodu-fg transition-colors">
            Docs
          </a>
          <a
            href="https://github.com/thadeu/voodu-caddy"
            target="_blank"
            rel="noreferrer"
            className="hover:text-voodu-fg transition-colors"
          >
            Plugins
          </a>
        </div>
      </div>
    </footer>
  );
}
