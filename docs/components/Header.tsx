'use client';

import { useCallback, useEffect, useRef, useState } from 'react';
import Link from 'next/link';

const navLinks = [
  { label: 'How it works', href: '/#how' },
  { label: 'Manifest', href: '/#hcl' },
  { label: 'Stack', href: '/#stack' },
  { label: 'Docs', href: '/docs' },
  { label: 'FAQ', href: '/#faq' },
];

export default function Header() {
  const [hidden, setHidden] = useState(false);
  const [menuOpen, setMenuOpen] = useState(false);
  const lastScrollY = useRef(0);

  const onScroll = useCallback(() => {
    const y = window.scrollY;

    setHidden(y > 64 && y > lastScrollY.current);

    lastScrollY.current = y;
  }, []);

  useEffect(() => {
    window.addEventListener('scroll', onScroll, { passive: true });

    return () => window.removeEventListener('scroll', onScroll);
  }, [onScroll]);

  useEffect(() => {
    if (menuOpen) {
      document.body.style.overflow = 'hidden';
    } else {
      document.body.style.overflow = '';
    }

    return () => {
      document.body.style.overflow = '';
    };
  }, [menuOpen]);

  return (
    <>
      <header
        className={`fixed top-0 left-0 right-0 z-50 transition-all duration-300 pt-[env(safe-area-inset-top)] backdrop-blur-md backdrop-saturate-150 bg-voodu-bg/78 border-b border-voodu-line ${
          hidden && !menuOpen ? '-translate-y-full' : 'translate-y-0'
        }`}
      >
        <div className="max-w-[1180px] mx-auto px-5 sm:px-8 md:px-10 lg:px-14 h-15 flex items-center justify-between">
          <Link href="/" className="flex items-center gap-2.5 font-bold tracking-tight text-[17px]">
            <span className="inline-grid place-items-center w-[18px] h-[18px] rounded bg-mint-400 text-[#07140d] font-mono font-extrabold text-[11px] leading-none">
              V
            </span>
            <span className="text-white">voodu</span>
          </Link>

          <nav className="hidden md:flex items-center gap-[22px] font-mono text-[13px] text-voodu-fg-dim">
            {navLinks.map(item => (
              <Link
                key={item.label}
                href={item.href}
                className="whitespace-nowrap transition-colors hover:text-voodu-fg"
              >
                {item.label}
              </Link>
            ))}
            <a
              href="https://github.com/thadeu/clowk-voodu"
              target="_blank"
              rel="noreferrer"
              className="inline-flex items-center gap-2 border border-voodu-line-strong px-3 py-1.5 rounded-full text-[12px] text-voodu-fg whitespace-nowrap transition-all hover:border-mint-400 hover:text-mint-400"
            >
              <span>★</span> github.com/thadeu/clowk-voodu
            </a>
          </nav>

          <button
            onClick={() => setMenuOpen(!menuOpen)}
            className="md:hidden relative w-8 h-8 flex flex-col items-center justify-center gap-1.5"
            aria-label="Toggle menu"
          >
            <span
              className={`block w-5 h-0.5 bg-white transition-all duration-300 origin-center ${
                menuOpen ? 'rotate-45 translate-y-2' : ''
              }`}
            />
            <span
              className={`block w-5 h-0.5 bg-white transition-all duration-300 ${
                menuOpen ? 'opacity-0 scale-0' : ''
              }`}
            />
            <span
              className={`block w-5 h-0.5 bg-white transition-all duration-300 origin-center ${
                menuOpen ? '-rotate-45 -translate-y-2' : ''
              }`}
            />
          </button>
        </div>
      </header>

      <div
        className={`fixed inset-0 z-40 bg-voodu-bg/98 backdrop-blur-lg transition-all duration-300 md:hidden ${
          menuOpen ? 'opacity-100 pointer-events-auto' : 'opacity-0 pointer-events-none'
        }`}
      >
        <div
          className={`flex flex-col items-center justify-center h-full gap-8 transition-all duration-300 ${
            menuOpen ? 'translate-y-0' : '-translate-y-8'
          }`}
        >
          {navLinks.map(item => (
            <Link
              key={item.label}
              href={item.href}
              onClick={() => setMenuOpen(false)}
              className="text-2xl font-semibold text-white/80 hover:text-white transition-colors"
            >
              {item.label}
            </Link>
          ))}

          <div className="w-12 h-px bg-white/10" />

          <a
            href="https://github.com/thadeu/clowk-voodu"
            target="_blank"
            rel="noreferrer"
            onClick={() => setMenuOpen(false)}
            className="inline-flex items-center gap-2 px-6 py-3 rounded-xl border border-mint-400 text-mint-400 font-mono text-sm hover:bg-mint-400 hover:text-[#07140d] transition-all"
          >
            ★ Star on GitHub
          </a>
        </div>
      </div>
    </>
  );
}
