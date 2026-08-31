import type { Metadata } from 'next';
import Link from 'next/link';
import Header from '@/components/Header';
import Footer from '@/components/Footer';

export const metadata: Metadata = {
  title: 'Single sign-on with Clowk — Voodu',
  description:
    'Voodu ships anonymous by default and lets the perimeter authenticate. Clowk adds identity per person, roles you can revoke, and a handover that cannot lock you out.',
};

const CLOWK = 'https://clowk.in';
const CLOWK_APP = 'https://app.clowk.in/';

/* The roles are the ones Permissions enforces in voodu-webui. Stated as what
 * each one may DO rather than as a tier name, because that is the question an
 * admin is answering when they pick one for a teammate. */
const roles = [
  {
    name: 'member',
    what: 'Reads the servers they were granted. Changes nothing.',
    for: 'The person on call who needs the logs, not the keys.',
  },
  {
    name: 'admin',
    what: 'Runs the org day to day: servers, tokens, alerts, dashboards, and who may see which server.',
    for: 'Whoever actually operates the fleet.',
  },
  {
    name: 'owner',
    what: 'The irreversible and the contractual: people, the org itself, the account, and deleting a server.',
    for: 'The one or two people who answer for the bill and the blast radius.',
  },
];

export default function SsoPage() {
  return (
    <>
      <Header />

      <main>
        <Hero />
        <Default />
        <WhatChanges />
        <Handover />
        <Roles />
        <Deploy />
        <Questions />
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

function PrimaryCTA({ href, children }: { href: string; children: React.ReactNode }) {
  return (
    <a
      href={href}
      target="_blank"
      rel="noreferrer"
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

  if (href.startsWith('#') || href.startsWith('http')) {
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

function Card({ tag, h, children }: { tag: string; h: string; children: React.ReactNode }) {
  return (
    <div className="border border-voodu-line rounded-xl px-5 py-4.5 bg-white/[0.01] transition-all hover:border-mint-400/40">
      <div className="font-mono text-[10.5px] text-mint-400 tracking-[0.08em] uppercase mb-2">{tag}</div>
      <h3 className="m-0 mb-1.5 font-sans font-semibold text-[16px] tracking-[-0.01em] text-white">{h}</h3>
      <p className="m-0 text-voodu-fg-dim text-[13.5px] leading-[1.55]">{children}</p>
    </div>
  );
}

function Hero() {
  return (
    <section className="pt-28 sm:pt-32 pb-20 sm:pb-24">
      <div className="max-w-[1180px] mx-auto px-5 sm:px-8 md:px-10 lg:px-14">
        <Eyebrow>single sign-on</Eyebrow>
        <h1 className="font-sans font-semibold text-[clamp(34px,5.5vw,64px)] tracking-[-0.03em] leading-[1.02] mb-5 text-balance text-white max-w-[20ch]">
          Stop sharing one way in.
        </h1>
        <p className="text-voodu-fg-dim max-w-[64ch] text-[18px] leading-[1.6] mb-8">
          Voodu ships anonymous: whoever reaches the port is the operator, and a VPN or access proxy in front of it is
          what does the authenticating. That is a real deployment shape and it stays supported. It stops being enough
          the second person joins — because &ldquo;who restarted that pod&rdquo; has no answer when everyone is the
          same account.
        </p>

        <div className="inline-flex gap-3 flex-wrap">
          <PrimaryCTA href={CLOWK}>See plans on clowk.in</PrimaryCTA>
          <SecondaryCTA href="#handover">How the switch works</SecondaryCTA>
        </div>

        <p className="mt-5 text-voodu-fg-mute font-mono text-[12px]">
          Free for one app · from $9/month for more · nothing moves until you confirm
        </p>
      </div>
    </section>
  );
}

function Default() {
  return (
    <Section id="anonymous">
      <Eyebrow>the default</Eyebrow>
      <H2>Anonymous mode is not a bypass. That matters.</H2>
      <Lede>
        With no sign-in configured, Voodu does not skip authorization — it provisions one real local operator, with a
        real account, org and owner membership. Every permission check runs down the same single path it always runs
        down; there simply happens to be one membership to find. There is no second code path to get wrong later.
      </Lede>

      <div className="grid grid-cols-1 md:grid-cols-3 gap-3">
        <Card tag="what you get" h="One command, no configuration">
          A single <span className="font-mono text-voodu-fg">docker run</span> and a volume. No identity provider to
          register, no keys to rotate, no login screen between an operator and an incident.
        </Card>
        <Card tag="who you are" h="One operator, owning one org">
          The Free Tier account. It owns the workspace, registers servers, and holds the controller tokens — exactly
          like a signed-in owner would.
        </Card>
        <Card tag="the catch" h="Whoever reaches the port is that owner">
          And an owner can reveal a controller token, which is not a metric — it is deploy, exec and logs on the box.
          Expose port 3000 without a perimeter in front and you are not leaking a chart, you are handing over the
          infrastructure.
        </Card>
      </div>

      <div className="mt-8 border border-mint-400/25 bg-mint-400/[0.04] rounded-xl px-5 py-4.5 max-w-[80ch]">
        <p className="m-0 text-voodu-fg-dim text-[14px] leading-[1.6]">
          <span className="text-mint-400 font-semibold">The dashboard says this out loud.</span> When it is running
          anonymous and a request arrives from an address that is not private, it shows a standing warning rather than
          waiting for someone to read the docs. Put Twingate, Tailscale, Cloudflare Access or your own VPN in front,
          or give people real identities — below.
        </p>
      </div>
    </Section>
  );
}

function WhatChanges() {
  return (
    <Section id="identity">
      <Eyebrow>what clowk adds</Eyebrow>
      <H2>People arrive as themselves.</H2>
      <Lede>
        Clowk is the identity layer. Voodu does not implement passwords, sessions or provider integrations — it
        mirrors a verified subject onto a local user and then answers the only question it cares about: what may this
        person do here.
      </Lede>

      <div className="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-3 gap-3">
        <Card tag="providers" h="Google, Apple, GitHub and X">
          People sign in with an account they already have and already protect with their own second factor. Nothing
          new to issue, nothing new to reset.
        </Card>
        <Card tag="revocation" h="Removing someone takes effect immediately">
          Access is read from the database on every request, so a removed membership is refused on the very next
          click — not at the next session expiry.
        </Card>
        <Card tag="sessions" h="And their session ends too">
          With a secret key configured, Voodu also asks Clowk to end that person&apos;s sessions. Best effort by
          design: a broker outage must never stop an admin from removing somebody.
        </Card>
        <Card tag="invites" h="Invitations instead of a shared door">
          Bring a teammate in with the role they need. An invitation nobody has accepted grants nothing — being asked
          is not the same as being inside.
        </Card>
        <Card tag="attribution" h="Actions have an author">
          Who registered that server, who rotated that token, who turned sign-in on. On a shared login all of it reads
          as &ldquo;the operator&rdquo;.
        </Card>
        <Card tag="scope" h="Per-server access, not all-or-nothing">
          A member can be granted the two servers they are on call for, rather than the whole fleet, and the org-wide
          surfaces stay with the people who run the org.
        </Card>
      </div>
    </Section>
  );
}

function Handover() {
  return (
    <Section id="handover">
      <Eyebrow>the switch</Eyebrow>
      <H2>Nothing moves until you have actually signed in.</H2>
      <Lede>
        This is the step that goes wrong in every product that does it naively. Anonymous mode runs as one local
        operator; a first Clowk sign-in creates a user keyed on the identity provider&apos;s subject. Do the handover
        eagerly and that first real sign-in produces a brand-new user with no membership — while every server, every
        token and every dashboard stays behind an account nobody can reach any more.
      </Lede>

      <figure className="m-0">
        <img
          src="/sso/screen.png"
          alt="The Single sign-on screen: an Authentication card showing Anonymous with fields for a publishable key and the owner email, beside an Identity panel explaining Clowk"
          className="w-full h-auto rounded-2xl border border-voodu-line"
        />
        <figcaption className="mt-3 text-voodu-fg-mute text-[12.5px] font-mono">
          Dashboard → account menu → SSO
        </figcaption>
      </figure>

      <div className="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-4 gap-3 mt-10">
        {[
          {
            n: '1',
            h: 'You record the claim',
            p: 'Paste the publishable key and the address that will own the workspace. This writes down who MAY claim it. It does not move anything.',
          },
          {
            n: '2',
            h: 'Sign-in turns on',
            p: 'The next request asks you to authenticate. Everything you registered is still there, still owned by the anonymous operator.',
          },
          {
            n: '3',
            h: 'You sign in as that person',
            p: 'Clowk verifies you. Because the address matches the one recorded, the dashboard offers you the workspace and tells you exactly what it contains.',
          },
          {
            n: '4',
            h: 'You confirm, and it is yours',
            p: 'Orgs, servers, tokens and history transfer to your identity in one step. Sign-in is now required for everyone.',
          },
        ].map((s) => (
          <div key={s.n} className="border border-voodu-line rounded-xl px-5 py-4.5 bg-white/[0.01]">
            <div className="font-mono text-[10.5px] text-mint-400 tracking-[0.08em] uppercase mb-2">step {s.n}</div>
            <h3 className="m-0 mb-1.5 font-sans font-semibold text-[16px] tracking-[-0.01em] text-white">{s.h}</h3>
            <p className="m-0 text-voodu-fg-dim text-[13.5px] leading-[1.55]">{s.p}</p>
          </div>
        ))}
      </div>

      <div className="mt-8 border border-mint-400/25 bg-mint-400/[0.04] rounded-xl px-5 py-4.5 max-w-[80ch]">
        <p className="m-0 text-voodu-fg-dim text-[14px] leading-[1.6]">
          <span className="text-mint-400 font-semibold">You cannot lock yourself out.</span> Environment variables
          always beat what is stored in the database, so a wrong publishable key is recoverable from the host: restart
          with <span className="font-mono text-voodu-fg">CLOWK_ENABLED=0</span> and you are back in as the local
          operator, with everything where you left it.
        </p>
      </div>
    </Section>
  );
}

function Roles() {
  return (
    <Section id="roles">
      <Eyebrow>roles</Eyebrow>
      <H2>Three of them, and each one is a sentence.</H2>
      <Lede>
        Roles are ordered, and every capability names the lowest role that holds it — so a permission check is a
        comparison rather than a list of exceptions that drifts. Anything not listed is denied.
      </Lede>

      <div className="border border-voodu-line rounded-2xl overflow-hidden">
        {roles.map((r) => (
          <div
            key={r.name}
            className="grid grid-cols-1 md:grid-cols-[140px_minmax(0,1.6fr)_minmax(0,1fr)] gap-2 md:gap-6 px-5 py-4 border-b border-voodu-line last:border-b-0"
          >
            <div className="font-mono text-[12.5px] text-mint-400">{r.name}</div>
            <div className="text-voodu-fg text-[14px] leading-[1.55]">{r.what}</div>
            <div className="text-voodu-fg-mute text-[13px] leading-[1.55]">{r.for}</div>
          </div>
        ))}
      </div>

      <p className="mt-6 text-voodu-fg-mute text-[13.5px] max-w-[70ch] leading-[1.6]">
        The same table decides what the interface draws. A control somebody may not use is not rendered — and the
        endpoint behind it refuses the request anyway, because a hidden button is decoration, not authorization.
      </p>
    </Section>
  );
}

function Deploy() {
  return (
    <Section id="deploy">
      <Eyebrow>configuring it</Eyebrow>
      <H2>From the screen, or from your environment.</H2>
      <Lede>
        The screen above is the right route for an installation that already exists and already has servers in it. For
        a box you rebuild from scratch, configure it the same way you configure everything else.
      </Lede>

      <div className="grid grid-cols-1 lg:grid-cols-2 gap-4 items-start">
        <Code label="docker run">{`docker run -d --name voodu-webui \\
  -p 3000:3000 \\
  -v voodu:/rails/storage \\
  -e CLOWK_ENABLED=1 \\
  -e CLOWK_PUBLISHABLE_KEY=pk_live_... \\
  ghcr.io/thadeu/voodu-webui`}</Code>

        <Code label="docker-compose.yml">{`services:
  webui:
    image: ghcr.io/thadeu/voodu-webui
    restart: unless-stopped
    ports:
      - "3000:3000"
    volumes:
      - voodu:/rails/storage
    environment:
      CLOWK_ENABLED: "1"
      CLOWK_PUBLISHABLE_KEY: pk_live_...

      # Optional. Your own auth domain, if you run one.
      CLOWK_SUBDOMAIN_URL: https://auth.company.com

      # Optional. Lets Voodu end a removed person's Clowk
      # sessions as well as denying them here.
      CLOWK_SECRET_KEY: sk_live_...

volumes:
  voodu:`}</Code>
      </div>

      <div className="grid grid-cols-1 md:grid-cols-3 gap-3 mt-8">
        <Card tag="fails closed" h="A misconfigured box refuses to boot">
          Asking for sign-in in production without a publishable key does not silently fall back to letting everyone
          in. It stops, and says which variable is missing.
        </Card>
        <Card tag="fails safe" h="An upgrade never removes authentication">
          If a publishable key is set, sign-in stays on whether or not the flag is. Losing your login to a routine
          image bump is not a failure mode worth allowing.
        </Card>
        <Card tag="the way back" h="The flag is also the escape hatch">
          <span className="font-mono text-voodu-fg">CLOWK_ENABLED=0</span> returns the installation to anonymous, with
          the workspace intact. Make sure the perimeter is back in front of it first.
        </Card>
      </div>
    </Section>
  );
}

function Questions() {
  return (
    <Section id="questions">
      <Eyebrow>the awkward questions</Eyebrow>
      <H2>Before you turn it on.</H2>

      <div className="grid grid-cols-1 md:grid-cols-2 gap-x-10 gap-y-8 max-w-[92ch]">
        {[
          {
            q: 'Do we have to use Clowk?',
            a: 'No. Anonymous behind your own perimeter is a supported, complete deployment — it is the default. Clowk is what you reach for when the perimeter can tell you someone is an employee but not which one.',
          },
          {
            q: 'What does it cost?',
            a: 'Creating an account is free, and one app is free — which is what a single Voodu installation is. From $9 a month if you need more than that.',
          },
          {
            q: 'Does Voodu see our passwords?',
            a: 'It never sees them. Voodu verifies a signed token and mirrors the subject onto a local user row. Credentials, second factors and provider integrations stay with Clowk.',
          },
          {
            q: 'What happens to what we already registered?',
            a: 'It transfers to you in one confirmed step, after you have proven you can sign in. Until then it stays exactly where it is — that ordering is the whole design.',
          },
          {
            q: 'Can we turn it off again?',
            a: 'Yes, from the screen while you are still signed in, or from the host with an environment variable. Nothing is deleted either way.',
          },
          {
            q: 'Is this an Enterprise feature?',
            a: (
              <>
                Sign-in itself is not gated. The free tier caps how many people you can invite, so identity for a team
                pairs with{' '}
                <Link href="/license/enterprise" className="text-mint-400 hover:underline">
                  an Enterprise licence
                </Link>{' '}
                in practice.
              </>
            ),
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
          Give the second person their own login.
        </h2>
        <p className="text-voodu-fg-dim max-w-[52ch] mx-auto text-[16px] leading-[1.6] mb-8">
          Free to create an account and free for one app. Paste the key into your dashboard when you are ready —
          nothing moves until you sign in and confirm.
        </p>

        <div className="inline-flex gap-3 flex-wrap justify-center">
          <PrimaryCTA href={CLOWK}>See plans on clowk.in</PrimaryCTA>
          <SecondaryCTA href={CLOWK_APP}>Create an account</SecondaryCTA>
        </div>
      </div>
    </section>
  );
}
