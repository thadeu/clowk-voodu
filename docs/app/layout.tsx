import type { Metadata, Viewport } from 'next';
import { IBM_Plex_Sans, IBM_Plex_Mono } from 'next/font/google';
import './globals.css';

const plexSans = IBM_Plex_Sans({
  subsets: ['latin'],
  weight: ['400', '500', '600', '700'],
  variable: '--font-ibm-plex-sans',
  display: 'swap',
});

const plexMono = IBM_Plex_Mono({
  subsets: ['latin'],
  weight: ['400', '500', '600'],
  variable: '--font-ibm-plex-mono',
  display: 'swap',
});

const SITE_URL = 'https://voodu.clowk.in';
const OG_IMAGE = `${SITE_URL}/og-image.png`;

export const metadata: Metadata = {
  title: 'Voodu — Self-hosted PaaS, commitless deploys',
  description:
    'Voodu is a Heroku-shaped, Kubernetes-honest deploy tool you run on your own boxes. One HCL file, one voodu apply, no git push.',
  metadataBase: new URL(SITE_URL),
  alternates: {
    canonical: '/',
  },
  openGraph: {
    title: 'Voodu — Self-hosted PaaS, commitless deploys',
    description:
      'One HCL file describes the running system. voodu apply does build, ship, route, swap. No bare repo, no git push, no plugin sprawl.',
    siteName: 'Voodu',
    url: SITE_URL,
    type: 'website',
    locale: 'en_US',
    images: [
      {
        url: OG_IMAGE,
        width: 1200,
        height: 630,
        alt: 'Voodu — Self-hosted PaaS, commitless deploys',
        type: 'image/png',
      },
    ],
  },
  twitter: {
    card: 'summary_large_image',
    title: 'Voodu — Self-hosted PaaS, commitless deploys',
    description: 'One HCL file. One voodu apply. No git push. 100% self-hosted, MIT.',
    images: [OG_IMAGE],
  },
  manifest: '/manifest.json',
  appleWebApp: {
    capable: true,
    title: 'Voodu',
    statusBarStyle: 'black-translucent',
    startupImage: ['/icons/icon-512.png'],
  },
  other: {
    'mobile-web-app-capable': 'yes',
  },
  robots: {
    index: true,
    follow: true,
    googleBot: {
      index: true,
      follow: true,
      'max-video-preview': -1,
      'max-image-preview': 'large',
      'max-snippet': -1,
    },
  },
};

export const viewport: Viewport = {
  themeColor: [
    { media: '(prefers-color-scheme: light)', color: '#0b0d0c' },
    { media: '(prefers-color-scheme: dark)', color: '#0b0d0c' },
  ],
  width: 'device-width',
  initialScale: 1,
  maximumScale: 1,
  userScalable: false,
  viewportFit: 'cover',
};

export default function RootLayout({
  children,
}: Readonly<{
  children: React.ReactNode;
}>) {
  return (
    <html lang="en" suppressHydrationWarning className={`${plexSans.variable} ${plexMono.variable} dark`}>
      <head>
        <meta
          name="viewport"
          content="width=device-width, initial-scale=1, maximum-scale=1, user-scalable=no, viewport-fit=cover"
        />
      </head>
      <body className="min-h-full flex flex-col antialiased bg-voodu-bg text-voodu-fg font-sans">{children}</body>
    </html>
  );
}
