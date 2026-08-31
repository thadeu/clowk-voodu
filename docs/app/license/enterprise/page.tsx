import type { Metadata } from 'next';
import Link from 'next/link';
import Header from '@/components/Header';
import Footer from '@/components/Footer';

export const metadata: Metadata = {
  title: 'Voodu Enterprise — licence',
  description:
    'Lift the free-tier caps, put the control plane in your own Postgres, and keep every byte on your own infrastructure. Offline licence, no phone-home.',
};

const CONTACT = 'mailto:hello@clowk.in?subject=Voodu%20Enterprise';

/* The numbers are the ones the app actually enforces (Entitlements::FREE and
 * Entitlements::LICENSED in voodu-webui). If they change there, they change
 * here — a pricing page that overstates the product is worse than no page. */
const limits = [
  { what: 'Accounts', free: '1', ent: 'Unlimited' },
  { what: 'Orgs', free: '1', ent: 'Unlimited' },
  { what: 'Member invites', free: 'None', ent: 'Unlimited' },
  { what: 'Searchable history', free: '3 days', ent: '90 days' },
  { what: 'Control-plane database', free: 'SQLite', ent: 'SQLite or your Postgres' },
  { what: 'Single sign-on', free: 'Perimeter only', ent: 'Clowk SSO' },
];

export default function EnterprisePage() {
  return (
    <>
      <Header />

      <main>
        <Hero />
        <Limits />
        <Security />
        <Activation />
        <Deploy />
        <Expiry />
        <EndCTA />
      </main>

      <Footer />
    </>
  );
}

function Eyebrow({ children }: { children: string }) {
  return (
    <div className="font-mono text-[12px] tracking-[0.08em] uppercase text-mint-400 mb-3.5">{`// ${children}`}</div>
  );
}

function Section({ id, children }: { id?: string; children: React.ReactNode }) {
  return (
    <section id={id} className="py-20 sm:py-24 border-t border-voodu-line">
      <div className="max-w-[1180px] mx-auto px-5 sm:px-8 md:px-10 lg:px-14">{children}</div>
    </section>
  );
}

function H2({ children }: { children: React.ReactNode }) {
  return (
    <h2 className="font-sans font-semibold text-[clamp(28px,4vw,44px)] tracking-[-0.025em] leading-[1.05] mb-4 text-balance text-white">
      {children}
    </h2>
  );
}

function Lede({ children }: { children: React.ReactNode }) {
  return <p className="text-voodu-fg-dim max-w-[62ch] text-[17px] leading-[1.6] mb-10">{children}</p>;
}

function PrimaryCTA({ children }: { children: React.ReactNode }) {
  return (
    <a
      href={CONTACT}
      className="inline-flex items-center gap-2 px-4 py-3 rounded-[10px] bg-mint-400 border border-mint-400 text-[#07140d] font-mono text-[13px] font-semibold whitespace-nowrap hover:brightness-110 transition-all"
    >
      {children}
    </a>
  );
}

function SecondaryCTA({ href, children }: { href: string; children: React.ReactNode }) {
  const className =
    'inline-flex items-center gap-2 px-4 py-3 rounded-[10px] border border-voodu-line bg-white/[0.02] ' +
    'text-voodu-fg font-mono text-[13px] font-semibold whitespace-nowrap hover:border-mint-400 ' +
    'hover:text-mint-400 transition-all';

  // Internal routes go through next/link; an in-page anchor is not a route.
  if (href.startsWith('#')) {
    return (
      <a href={href} className={className}>
        {children}
      </a>
    );
  }

  return (
    <Link href={href} className={className}>
      {children}
    </Link>
  );
}

function Code({ label, children }: { label: string; children: string }) {
  return (
    <div className="bg-voodu-code border border-voodu-line rounded-2xl overflow-hidden">
      <div className="px-4 py-2.5 border-b border-voodu-line font-mono text-[11px] tracking-[0.06em] uppercase text-voodu-fg-mute">
        {label}
      </div>
      <pre className="m-0 px-4 py-4 overflow-x-auto font-mono text-[12.5px] leading-[1.7] text-voodu-fg-dim">
        <code>{children}</code>
      </pre>
    </div>
  );
}

function Hero() {
  return (
    <section className="pt-28 sm:pt-32 pb-20 sm:pb-24">
      <div className="max-w-[1180px] mx-auto px-5 sm:px-8 md:px-10 lg:px-14">
        <Eyebrow>enterprise</Eyebrow>
        <h1 className="font-sans font-semibold text-[clamp(34px,5.5vw,64px)] tracking-[-0.03em] leading-[1.02] mb-5 text-balance text-white max-w-[20ch]">
          Your infrastructure. Your database. Your network.
        </h1>
        <p className="text-voodu-fg-dim max-w-[64ch] text-[18px] leading-[1.6] mb-8">
          Voodu is self-hosted either way — Enterprise does not move your data somewhere else, because there is
          nowhere else. What it lifts are the caps the free tier puts on a single operator running a single box, and
          it lets the control plane live in a database your DBA already backs up.
        </p>

        <div className="inline-flex gap-3 flex-wrap">
          <PrimaryCTA>Talk to us about Enterprise</PrimaryCTA>
          <SecondaryCTA href="#activation">See how activation works</SecondaryCTA>
        </div>

        <p className="mt-5 text-voodu-fg-mute font-mono text-[12px]">
          Offline licence · no phone-home · nothing to open outbound
        </p>

        <ProductShot />
      </div>
    </section>
  );
}

// The dashboard itself, under the pitch rather than beside it.
//
// A two-column hero would have put this in half the width, and at 2000px of
// charts, pod rows and alert thresholds it is a picture whose whole argument is
// the detail — shrunk, it becomes a grey rectangle that says "a dashboard
// exists". Full width, directly after the CTA, is where it answers the question
// the CTA just raised.
//
// The bottom fade hands the image off into the section below instead of
// stopping at a hard edge. There was a radial glow above it too, and it went:
// its brightest point sat over empty background ABOVE the figure, so it read as
// a grey smudge floating between the footnote and the screenshot rather than as
// light behind anything. The rest of this site is flat — borders, no glows —
// and the flourish was mine, not the design's.
function ProductShot() {
  return (
    <div className="relative mt-14 sm:mt-16">
      <figure className="relative m-0 rounded-2xl border border-voodu-line overflow-hidden bg-voodu-code">
        <img
          src="/license/overview.png"
          width={2000}
          height={1200}
          fetchPriority="high"
          alt="The Voodu dashboard overview: CPU, memory and disk cards with sparklines, a table of running pods, recent incidents, alerts and dashboards"
          className="block w-full h-auto"
        />
        <div
          aria-hidden="true"
          className="pointer-events-none absolute inset-x-0 bottom-0 h-20 sm:h-28 bg-gradient-to-b from-transparent to-voodu-bg"
        />
      </figure>

      <figcaption className="mt-3.5 text-voodu-fg-mute text-[12.5px] font-mono">
        One server, three pods. The same screen at forty.
      </figcaption>
    </div>
  );
}

function Limits() {
  return (
    <Section id="limits">
      <Eyebrow>what changes</Eyebrow>
      <H2>The free tier is a complete product. It is sized for one person.</H2>
      <Lede>
        One operator, one org, one box, three days of history. That is a real tool and it stays free. It stops being
        enough the moment a second person needs their own login, or a second team needs their own org, or an incident
        review asks what happened last month.
      </Lede>

      <div className="border border-voodu-line rounded-2xl overflow-hidden">
        <div className="grid grid-cols-[minmax(0,1.4fr)_minmax(0,1fr)_minmax(0,1.3fr)] gap-3 px-5 py-3 border-b border-voodu-line bg-white/[0.02] font-mono text-[11px] tracking-[0.06em] uppercase text-voodu-fg-mute">
          <div>Limit</div>
          <div>Free</div>
          <div className="text-mint-400">Enterprise</div>
        </div>

        {limits.map((row) => (
          <div
            key={row.what}
            className="grid grid-cols-[minmax(0,1.4fr)_minmax(0,1fr)_minmax(0,1.3fr)] gap-3 px-5 py-3.5 border-b border-voodu-line last:border-b-0 text-[13.5px]"
          >
            <div className="text-voodu-fg">{row.what}</div>
            <div className="text-voodu-fg-mute font-mono text-[12.5px]">{row.free}</div>
            <div className="text-mint-400 font-mono text-[12.5px]">{row.ent}</div>
          </div>
        ))}
      </div>

      <p className="mt-6 text-voodu-fg-mute text-[13.5px] max-w-[70ch] leading-[1.6]">
        Telemetry — metrics, logs and HEP — stays in SQLite on the container&apos;s volume in both tiers, and rebuilds
        itself from your controllers if you lose it. Only the control plane (orgs, people, servers, encrypted tokens)
        moves to Postgres, because that is the part you cannot re-derive.
      </p>
    </Section>
  );
}

const guarantees = [
  {
    tag: 'offline',
    h: 'The licence never calls home',
    p: 'It is a signed token your installation verifies locally with a public key baked into the image. No licence server, no outbound connection, no telemetry back to us. It works in an air-gapped network, and there is nothing for us to switch off.',
  },
  {
    tag: 'no lock-in',
    h: 'It cannot be revoked out from under you',
    p: 'Being offline is the trade in both directions: your licence is valid until the expiry it was signed with. Nobody can reach in and end it early — not us, not somebody who compromises us.',
  },
  {
    tag: 'no data loss',
    h: 'Lapsing hides, it never deletes',
    p: 'Retention has two numbers: how long you keep telemetry, and how far back the UI will search. A lapsed licence only lowers the second one. The rows are still on your volume, and renewing shows them again.',
  },
  {
    tag: 'grace',
    h: 'Expiry is a slope, not a cliff',
    p: 'Entitlements stay live for 30 days past the expiry date. A renewal that lands late, or a purchase order stuck in someone’s approvals, does not take out the dashboard your team is watching an incident on.',
  },
  {
    tag: 'your data',
    h: 'Nothing leaves your network',
    p: 'The dashboard talks to your controllers and to your database. There is no vendor tenancy, no shared analytics, no support backdoor. What your operators see, they see from your own box.',
  },
  {
    tag: 'credentials',
    h: 'The controller token is never in flight',
    p: 'Controller access tokens are encrypted at rest, and requests to the PAT plane carry a per-request HMAC signature instead of the token itself. Capturing a request gets you that one request, already used — not a credential.',
  },
];

function Security() {
  return (
    <Section id="security">
      <Eyebrow>the security review</Eyebrow>
      <H2>Written for the person who has to sign off on it.</H2>
      <Lede>
        Buying an observability tool usually means handing a vendor a path into production. This one is the opposite
        shape, and the reasons are structural rather than promises — they are consequences of how the licence and the
        transport are built.
      </Lede>

      <div className="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-3 gap-3">
        {guarantees.map((g) => (
          <div
            key={g.tag}
            className="border border-voodu-line rounded-xl px-5 py-4.5 bg-white/[0.01] transition-all hover:border-mint-400/40"
          >
            <div className="font-mono text-[10.5px] text-mint-400 tracking-[0.08em] uppercase mb-2">{g.tag}</div>
            <h3 className="m-0 mb-1.5 font-sans font-semibold text-[16px] tracking-[-0.01em] text-white">{g.h}</h3>
            <p className="m-0 text-voodu-fg-dim text-[13.5px] leading-[1.55]">{g.p}</p>
          </div>
        ))}
      </div>

      <p className="mt-8 text-voodu-fg-mute text-[13.5px] max-w-[70ch] leading-[1.6]">
        One thing this page will not claim: traffic to your controllers on port 8687 is plain HTTP. The credential is
        protected by the request signature; the response bodies are not. That is fine inside a private network and is
        not fine across the public internet — put the plane behind your VPN or mesh, the same as you would any other
        admin API.
      </p>
    </Section>
  );
}

function Activation() {
  return (
    <Section id="activation">
      <Eyebrow>activation</Eyebrow>
      <H2>Paste it in. Nothing restarts.</H2>
      <Lede>
        You already have the installation. Enterprise is a token you paste into it — there is no separate build, no
        migration, and no window where the dashboard is down. The screen below is the free tier: the licence goes in
        the box on the left, and the panel on the right disappears the moment it verifies.
      </Lede>

      <figure className="m-0">
        <img
          src="/license/activate.png"
          width={2000}
          height={1010}
          loading="lazy"
          alt="The License screen on the free tier: a Plan card showing Free with its limits and an Activate a licence field, beside an Enterprise panel"
          className="w-full h-auto rounded-2xl border border-voodu-line"
        />
        <figcaption className="mt-3 text-voodu-fg-mute text-[12.5px] font-mono">
          Dashboard → account menu → License
        </figcaption>
      </figure>

      <div className="grid grid-cols-1 md:grid-cols-3 gap-3 mt-10">
        {[
          {
            n: '1',
            h: 'The signature is checked locally',
            p: 'Against a public key already in the image. If it does not verify, nothing changes and the screen says why.',
          },
          {
            n: '2',
            h: 'The new limits apply on the next request',
            p: 'No restart, no redeploy. The operator watching a dashboard sees the caps lift without losing their page.',
          },
          {
            n: '3',
            h: 'The activation is recorded',
            p: 'Who pasted it, when, and for which customer — kept as history, so a renewal a year later has an audit trail.',
          },
        ].map((s) => (
          <div key={s.n} className="border border-voodu-line rounded-xl px-5 py-4.5 bg-white/[0.01]">
            <div className="font-mono text-[10.5px] text-mint-400 tracking-[0.08em] uppercase mb-2">step {s.n}</div>
            <h3 className="m-0 mb-1.5 font-sans font-semibold text-[16px] tracking-[-0.01em] text-white">{s.h}</h3>
            <p className="m-0 text-voodu-fg-dim text-[13.5px] leading-[1.55]">{s.p}</p>
          </div>
        ))}
      </div>

      <p className="mt-8 text-voodu-fg-mute text-[13.5px] max-w-[70ch] leading-[1.6]">
        Pasting is the fastest route and the wrong one for a fleet you rebuild from scratch. For that, ship the licence
        as configuration — below.
      </p>
    </Section>
  );
}

function Deploy() {
  return (
    <Section id="deploy">
      <Eyebrow>deploying it</Eyebrow>
      <H2>Or ship it as configuration.</H2>
      <Lede>
        A licence supplied by environment is read at boot, so a container that is destroyed and recreated comes back
        licensed with no manual step. Whichever source is newer wins, so a token pasted into the UI later still takes
        precedence over a stale one in your environment.
      </Lede>

      <div className="grid grid-cols-1 lg:grid-cols-2 gap-4 items-start">
        <Code label="docker run — inline">{`docker run -d --name voodu-webui \\
  -p 3000:3000 \\
  -v voodu:/rails/storage \\
  -e VOODU_LICENSE="eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9..." \\
  ghcr.io/thadeu/voodu-webui`}</Code>

        <Code label="docker run — from a mounted secret">{`docker run -d --name voodu-webui \\
  -p 3000:3000 \\
  -v voodu:/rails/storage \\
  -v /etc/voodu/license.jws:/run/secrets/voodu-license:ro \\
  -e VOODU_LICENSE_FILE=/run/secrets/voodu-license \\
  ghcr.io/thadeu/voodu-webui`}</Code>
      </div>

      <p className="text-voodu-fg-dim text-[14px] leading-[1.6] max-w-[70ch] mt-8 mb-4">
        The compose file below is the shape most Enterprise installations run: the licence as a file secret, and the
        control plane in Postgres. Telemetry stays on the volume in both cases.
      </p>

      <Code label="docker-compose.yml">{`services:
  webui:
    image: ghcr.io/thadeu/voodu-webui
    restart: unless-stopped
    ports:
      - "3000:3000"
    volumes:
      - voodu:/rails/storage
      - ./license.jws:/run/secrets/voodu-license:ro
    environment:
      VOODU_LICENSE_FILE: /run/secrets/voodu-license

      # Control plane in your own Postgres. Omit it and everything
      # runs on SQLite in the volume — still a supported Enterprise
      # deployment, just one you back up differently.
      DATABASE_URL: postgres://voodu:CHANGE_ME@postgres:5432/voodu

      # How long telemetry is KEPT. The licence caps how far back the
      # UI will search; this caps what is on disk at all.
      VOODU_RETENTION_DAYS: "90"
    depends_on:
      postgres:
        condition: service_healthy

  postgres:
    image: postgres:18-alpine
    restart: unless-stopped
    environment:
      POSTGRES_USER: voodu
      POSTGRES_PASSWORD: CHANGE_ME
      POSTGRES_DB: voodu
    volumes:
      - pgdata:/var/lib/postgresql/data
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U voodu"]
      interval: 5s
      timeout: 3s
      retries: 10

volumes:
  voodu:
  pgdata:`}</Code>

      <p className="mt-6 text-voodu-fg-mute text-[13.5px] max-w-[70ch] leading-[1.6]">
        There is no secret key to generate: the container creates and persists its own on first boot, in the volume.
        Migrations run on boot and are idempotent, so upgrading is <span className="font-mono">pull</span> and{' '}
        <span className="font-mono">up -d</span> — keep the volume and nothing else is needed.
      </p>
    </Section>
  );
}

function Expiry() {
  return (
    <Section id="expiry">
      <Eyebrow>the awkward questions</Eyebrow>
      <H2>What happens when it runs out.</H2>

      <div className="grid grid-cols-1 md:grid-cols-2 gap-x-10 gap-y-8 max-w-[92ch]">
        {[
          {
            q: 'Does the dashboard stop?',
            a: 'No. Entitlements stay live for 30 days past expiry, and after that the installation falls back to the free tier. It keeps running, keeps polling, keeps showing you your servers.',
          },
          {
            q: 'Do we lose the data we collected?',
            a: 'No. Telemetry lives on your volume under your own retention setting. A lapsed licence lowers how far back the UI searches, not what is stored — renewing brings the window back.',
          },
          {
            q: 'Are we forced off Postgres?',
            a: 'No. Losing a licence never disconnects a database that is holding your production control plane. That would be a data-loss event dressed up as enforcement.',
          },
          {
            q: 'What about the extra orgs and people?',
            a: 'They stay. What the free tier stops is creating more of them, not reaching the ones you have.',
          },
          {
            q: 'Can we try it before buying?',
            a: 'The free tier is the same binary, so there is nothing to trial except the caps. Ask and we will issue a time-boxed licence so you can size it against your own fleet.',
          },
          {
            q: 'What licence is the software under?',
            a: 'Elastic License 2.0. You can read it, run it, and modify it for your own use. What it stops is someone reselling it as a competing hosted product.',
          },
        ].map((f) => (
          <div key={f.q}>
            <h3 className="m-0 mb-2 font-sans font-semibold text-[16px] tracking-[-0.01em] text-white">{f.q}</h3>
            <p className="m-0 text-voodu-fg-dim text-[14px] leading-[1.6]">{f.a}</p>
          </div>
        ))}
      </div>
    </Section>
  );
}

function EndCTA() {
  return (
    <section className="border-t border-voodu-line py-24 text-center">
      <div className="max-w-[1180px] mx-auto px-5 sm:px-8 md:px-10 lg:px-14">
        <h2 className="font-sans font-semibold text-[clamp(30px,5vw,56px)] tracking-[-0.03em] leading-none mb-5 text-balance text-white">
          Tell us how big your fleet is.
        </h2>
        <p className="text-voodu-fg-dim max-w-[52ch] mx-auto text-[16px] leading-[1.6] mb-8">
          Pricing follows the size of what you are running, and the answer comes from a person rather than a form.
          Bring your security questionnaire — the interesting parts are answered above.
        </p>

        <div className="inline-flex gap-3 flex-wrap justify-center">
          <PrimaryCTA>Email hello@clowk.in</PrimaryCTA>
          <SecondaryCTA href="/docs">Read the docs</SecondaryCTA>
        </div>
      </div>
    </section>
  );
}
