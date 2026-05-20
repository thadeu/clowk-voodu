import { source } from '@/lib/source';
import { DocsLayout } from 'fumadocs-ui/layouts/docs';
import { RootProvider } from 'fumadocs-ui/provider/next';
import type { ReactNode } from 'react';
import { DocsSidebar } from '@/components/docs/docs-sidebar';

export default function Layout({ children }: { children: ReactNode }) {
  return (
    <RootProvider
      theme={{
        defaultTheme: 'dark',
        forcedTheme: 'dark',
      }}
    >
      <DocsSidebar />
      <DocsLayout
        tree={source.getPageTree()}
        nav={{ enabled: false }}
        sidebar={{ enabled: false }}
        containerProps={{
          className: 'docs-layout',
        }}
      >
        {children}
      </DocsLayout>
    </RootProvider>
  );
}
