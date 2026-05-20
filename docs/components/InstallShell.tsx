'use client';

import { useState } from 'react';

const CMD = 'curl -fsSL voodu.clowk.in/install | bash';

export default function InstallShell() {
  const [copied, setCopied] = useState(false);

  const onCopy = () => {
    navigator.clipboard?.writeText(CMD);
    setCopied(true);
    setTimeout(() => setCopied(false), 1400);
  };

  return (
    <button
      onClick={onCopy}
      title="Click to copy"
      className="inline-flex items-center gap-2.5 font-mono text-[13px] bg-voodu-bg-elev border border-voodu-line-strong px-3.5 py-3 rounded-[10px] text-voodu-fg cursor-copy transition-colors max-w-full overflow-hidden hover:border-mint-400"
    >
      <span className="text-mint-400 select-none">$</span>
      <span className="whitespace-nowrap overflow-hidden text-ellipsis">{CMD}</span>
      <span className="text-voodu-fg-mute font-mono text-[11px] border-l border-voodu-line pl-2.5">
        {copied ? 'copied' : 'copy'}
      </span>
    </button>
  );
}
