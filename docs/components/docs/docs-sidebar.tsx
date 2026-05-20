'use client';

import { useCallback, useEffect, useRef, useState } from 'react';
import Link from 'next/link';
import { usePathname } from 'next/navigation';
import { ChevronDown, X } from 'lucide-react';
import { contents, type SidebarSection } from './sidebar-content';

export function DocsSidebar() {
  const pathname = usePathname();
  const [mobileOpen, setMobileOpen] = useState(false);

  useEffect(() => {
    if (mobileOpen) {
      document.body.style.overflow = 'hidden';
    } else {
      document.body.style.overflow = '';
    }

    return () => {
      document.body.style.overflow = '';
    };
  }, [mobileOpen]);

  useEffect(() => {
    setMobileOpen(false);
  }, [pathname]);

  return (
    <>
      <button
        onClick={() => setMobileOpen(true)}
        className="docs-sidebar-trigger fixed top-3 left-3 z-50 flex items-center gap-2 rounded-lg bg-white/5 px-3 py-2 text-sm text-white/60 backdrop-blur-md border border-white/8 lg:hidden"
        aria-label="Open sidebar"
      >
        <svg width="16" height="16" viewBox="0 0 16 16" fill="none">
          <path d="M2 4h12M2 8h12M2 12h12" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" />
        </svg>
        Menu
      </button>

      <div
        className={`docs-sidebar-overlay fixed inset-0 z-40 bg-black/60 backdrop-blur-sm transition-opacity lg:hidden ${
          mobileOpen ? 'opacity-100' : 'opacity-0 pointer-events-none'
        }`}
        onClick={() => setMobileOpen(false)}
      />

      <aside
        className={`docs-sidebar fixed top-0 left-0 bottom-0 z-50 flex flex-col bg-voodu-bg border-r border-white/5 transition-transform duration-300 lg:translate-x-0 ${
          mobileOpen ? 'translate-x-0' : '-translate-x-full'
        }`}
      >
        <div className="flex items-center justify-between px-5 h-14 border-b border-white/5 shrink-0">
          <Link href="/docs" className="flex items-center gap-2">
            <span className="inline-grid place-items-center w-5 h-5 rounded bg-mint-400 text-[#07140d] font-mono font-extrabold text-[11px]">
              V
            </span>
            <span className="text-lg font-bold tracking-tight text-white">voodu</span>
            <span className="text-[10px] font-medium text-white/30 bg-white/5 px-1.5 py-0.5 rounded">Docs</span>
          </Link>

          <button
            onClick={() => setMobileOpen(false)}
            className="lg:hidden text-white/40 hover:text-white/70 transition-colors"
            aria-label="Close sidebar"
          >
            <X size={18} />
          </button>
        </div>

        <nav className="flex-1 overflow-y-auto px-3 py-3 docs-sidebar-scroll">
          {contents.map(section => (
            <SidebarSectionItem key={section.title} section={section} pathname={pathname} />
          ))}
        </nav>

        <div className="px-5 py-3 border-t border-white/5 shrink-0">
          <a
            href="https://github.com/thadeu/clowk-voodu"
            className="flex items-center gap-2 text-xs text-white/25 hover:text-white/50 transition-colors"
            target="_blank"
            rel="noopener noreferrer"
          >
            <svg className="w-4 h-4" fill="currentColor" viewBox="0 0 24 24">
              <path d="M12 2C6.477 2 2 6.484 2 12.017c0 4.425 2.865 8.18 6.839 9.504.5.092.682-.217.682-.483 0-.237-.008-.868-.013-1.703-2.782.605-3.369-1.343-3.369-1.343-.454-1.158-1.11-1.466-1.11-1.466-.908-.62.069-.608.069-.608 1.003.07 1.531 1.032 1.531 1.032.892 1.53 2.341 1.088 2.91.832.092-.647.35-1.088.636-1.338-2.22-.253-4.555-1.113-4.555-4.951 0-1.093.39-1.988 1.029-2.688-.103-.253-.446-1.272.098-2.65 0 0 .84-.27 2.75 1.026A9.564 9.564 0 0112 6.844c.85.004 1.705.115 2.504.337 1.909-1.296 2.747-1.027 2.747-1.027.546 1.379.202 2.398.1 2.651.64.7 1.028 1.595 1.028 2.688 0 3.848-2.339 4.695-4.566 4.943.359.309.678.92.678 1.855 0 1.338-.012 2.419-.012 2.747 0 .268.18.58.688.482A10.019 10.019 0 0022 12.017C22 6.484 17.522 2 12 2z" />
            </svg>
            GitHub
          </a>
        </div>
      </aside>
    </>
  );
}

function SidebarSectionItem({ section, pathname }: { section: SidebarSection; pathname: string }) {
  const isActive = section.list.some(item => pathname === item.href);
  const [open, setOpen] = useState(isActive);
  const contentRef = useRef<HTMLDivElement>(null);
  const activeRef = useRef<HTMLAnchorElement>(null);

  useEffect(() => {
    if (isActive && !open) {
      setOpen(true);
    }
  }, [isActive, open]);

  useEffect(() => {
    if (isActive && activeRef.current) {
      activeRef.current.scrollIntoView({ block: 'nearest' });
    }
  }, [isActive, pathname]);

  const Icon = section.icon;

  const toggleOpen = useCallback(() => {
    setOpen(prev => !prev);
  }, []);

  return (
    <div className="mb-1">
      <button
        onClick={toggleOpen}
        className="border-b border-white/6 w-full text-left flex gap-2 items-center px-4 py-2.5 transition-colors font-medium text-sm tracking-wider text-white bg-white/3"
      >
        <Icon size={16} className="opacity-50" />
        <span>{section.title}</span>
        <ChevronDown
          size={12}
          className={`ml-auto opacity-40 transition-transform duration-200 ${open ? 'rotate-0' : '-rotate-90'}`}
        />
      </button>

      <div
        ref={contentRef}
        className={`grid transition-all duration-200 ease-out ${
          open ? 'grid-rows-[1fr] opacity-100' : 'grid-rows-[0fr] opacity-0'
        }`}
      >
        <div className="overflow-hidden">
          <div className="pb-1">
            {section.list.map(item => {
              const active = pathname === item.href;
              const ItemIcon = item.icon;

              return (
                <Link
                  key={item.href}
                  href={item.href}
                  ref={active ? activeRef : undefined}
                  className={`relative flex items-center gap-2.5 px-4 py-1 text-[14px] transition-all duration-150 ${
                    active
                      ? 'text-mint-400 font-medium bg-mint-400/5'
                      : 'text-white/65 hover:text-white/90 hover:bg-white/3'
                  }`}
                >
                  {ItemIcon && <ItemIcon size={16} className={active ? 'opacity-80' : 'opacity-35'} />}
                  {item.title}
                </Link>
              );
            })}
          </div>
        </div>
      </div>
    </div>
  );
}
