import Link from 'next/link';

export default function NotFound() {
  return (
    <main className="min-h-screen bg-voodu-bg flex flex-col items-center justify-center px-6 text-center">
      <Link href="/" className="flex items-center gap-2 mb-12">
        <span className="inline-grid place-items-center w-6 h-6 rounded bg-mint-400 text-[#07140d] font-mono font-extrabold text-xs">
          V
        </span>
        <span className="text-xl font-bold tracking-tight text-white">voodu</span>
      </Link>

      <p className="text-8xl font-bold text-mint-400 mb-4">404</p>

      <h1 className="text-2xl md:text-3xl font-bold text-white mb-3">Page not found</h1>

      <p className="text-white/40 text-base max-w-md mb-10">
        The page you&apos;re looking for doesn&apos;t exist or has been moved.
      </p>

      <Link
        href="/"
        className="inline-flex items-center gap-2 px-6 py-3 rounded-xl bg-mint-400 text-[#07140d] font-semibold text-base hover:brightness-110 transition-all"
      >
        Back to home
        <svg className="w-4 h-4" fill="none" viewBox="0 0 24 24" stroke="currentColor" strokeWidth={2}>
          <path strokeLinecap="round" strokeLinejoin="round" d="M13 7l5 5m0 0l-5 5m5-5H6" />
        </svg>
      </Link>
    </main>
  );
}
