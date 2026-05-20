const cells = [
  { k: '1 file', v: 'voodu.hcl is the source of truth' },
  { k: '1 command', v: 'voodu apply does build, ship, route, swap' },
  { k: '0 git pushes', v: 'tarball over ssh, content-addressed' },
  { k: 'TLS by default', v: "Caddy + Let's Encrypt, on-demand wildcards" },
];

export default function Strip() {
  return (
    <div className="grid grid-cols-2 md:grid-cols-4 border-y border-voodu-line">
      {cells.map((c, i) => (
        <div
          key={i}
          className="px-5 sm:px-8 md:px-10 lg:px-14 py-5 border-r border-voodu-line last:border-r-0 font-mono text-[12px] text-voodu-fg-dim"
        >
          <strong className="block font-sans text-white font-semibold text-[22px] tracking-[-0.02em] mb-1">
            {c.k}
          </strong>
          {c.v}
        </div>
      ))}
    </div>
  );
}
