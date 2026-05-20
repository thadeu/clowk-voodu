import type { LucideIcon } from 'lucide-react';
import {
  Rocket,
  FileCode2,
  Terminal,
  Blocks,
  BookOpen,
  HelpCircle,
  ShieldCheck,
  HandHelping,
  AlertTriangle,
  Layers,
  Network,
  Database,
  Workflow,
  ScrollText,
  Box,
  PlayCircle,
  Settings2,
  Globe,
  Server,
  Container,
  KeyRound,
  RefreshCcw,
  GitBranch,
  Plug,
  Clock,
  Cpu,
  Activity,
  Sparkles,
  Scaling,
  Send,
  Hammer,
  Link2,
  Braces,
  Package,
  FileText,
  Gauge,
  Disc3,
  GitMerge,
  Webhook,
  FolderOpen,
  Sparkle,
  Lock,
  HeartPulse,
  Layers3,
  GitFork,
  Factory,
} from 'lucide-react';

export interface ListItem {
  title: string;
  href: string;
  icon?: LucideIcon;
}

export interface SidebarSection {
  title: string;
  icon: LucideIcon;
  list: ListItem[];
}

export const contents: SidebarSection[] = [
  {
    title: 'Getting Started',
    icon: Rocket,
    list: [
      { title: 'Introduction', href: '/docs', icon: BookOpen },
      { title: 'Install', href: '/docs/getting-started/install', icon: PlayCircle },
      { title: 'First deploy', href: '/docs/getting-started/first-deploy', icon: Server },
      { title: 'Remotes', href: '/docs/getting-started/remotes', icon: Globe },
    ],
  },
  {
    title: 'Architecture',
    icon: Cpu,
    list: [
      { title: 'Overview', href: '/docs/architecture/overview', icon: Layers },
      { title: 'Controller', href: '/docs/architecture/controller', icon: Server },
      { title: 'Reconciler', href: '/docs/architecture/reconciler', icon: GitMerge },
      { title: 'HTTP API', href: '/docs/architecture/api', icon: Webhook },
    ],
  },
  {
    title: 'Examples',
    icon: FolderOpen,
    list: [
      { title: 'Overview', href: '/docs/examples/overview', icon: Layers },
      { title: 'Hello world', href: '/docs/examples/hello-world', icon: Sparkle },
      { title: 'Ingress routing', href: '/docs/examples/ingress-routing', icon: Network },
      { title: 'Build modes', href: '/docs/examples/build-modes', icon: Hammer },
      { title: 'Private registry', href: '/docs/examples/private-registry', icon: Lock },
      { title: 'Assets', href: '/docs/examples/assets', icon: Package },
      { title: 'Health checks', href: '/docs/examples/health-checks', icon: HeartPulse },
      { title: 'Init containers', href: '/docs/examples/init-containers', icon: Sparkles },
      { title: 'Autoscale', href: '/docs/examples/autoscale', icon: Scaling },
      { title: 'On-deploy webhooks', href: '/docs/examples/on-deploy', icon: Webhook },
      { title: 'Stateful services', href: '/docs/examples/stateful-services', icon: Database },
      { title: 'Multi-environment', href: '/docs/examples/multi-environment', icon: Layers3 },
      { title: 'Shared scope', href: '/docs/examples/shared-scope', icon: GitFork },
      { title: 'Production stack', href: '/docs/examples/production-stack', icon: Factory },
    ],
  },
  {
    title: 'Manifests (HCL)',
    icon: FileCode2,
    list: [
      { title: 'Overview', href: '/docs/manifests/overview', icon: Layers },
      { title: 'Interpolation', href: '/docs/manifests/interpolation', icon: Braces },
      { title: 'deployment', href: '/docs/manifests/deployment', icon: Server },
      { title: 'app', href: '/docs/manifests/app', icon: Box },
      { title: 'statefulset', href: '/docs/manifests/statefulset', icon: Disc3 },
      { title: 'ingress & TLS', href: '/docs/manifests/ingress', icon: Network },
      { title: 'jobs & cronjobs', href: '/docs/manifests/jobs', icon: Workflow },
      { title: 'asset', href: '/docs/manifests/asset', icon: Package },
      { title: 'registry', href: '/docs/manifests/registry', icon: Container },
      { title: 'postgres', href: '/docs/manifests/postgres', icon: Database },
      { title: 'redis', href: '/docs/manifests/redis', icon: Database },
      { title: 'probes', href: '/docs/manifests/probes', icon: Activity },
      { title: 'init containers', href: '/docs/manifests/init', icon: Sparkles },
      { title: 'autoscale', href: '/docs/manifests/autoscale', icon: Scaling },
      { title: 'on_deploy', href: '/docs/manifests/on-deploy', icon: Webhook },
      { title: 'release', href: '/docs/manifests/release', icon: Send },
      { title: 'resources', href: '/docs/manifests/resources', icon: Gauge },
      { title: 'logs', href: '/docs/manifests/logs', icon: FileText },
      { title: 'build', href: '/docs/manifests/build', icon: Hammer },
      { title: 'depends_on', href: '/docs/manifests/depends-on', icon: Link2 },
      { title: 'config & secrets', href: '/docs/manifests/config', icon: KeyRound },
    ],
  },
  {
    title: 'CLI',
    icon: Terminal,
    list: [
      { title: 'apply', href: '/docs/cli/apply', icon: RefreshCcw },
      { title: 'diff', href: '/docs/cli/diff', icon: GitBranch },
      { title: 'logs', href: '/docs/cli/logs', icon: ScrollText },
      { title: 'config', href: '/docs/cli/config', icon: Settings2 },
      { title: 'remote', href: '/docs/cli/remote', icon: Globe },
      { title: 'plugins', href: '/docs/cli/plugins', icon: Plug },
    ],
  },
  {
    title: 'Plugins',
    icon: Blocks,
    list: [
      { title: 'voodu-postgres', href: '/docs/plugins/postgres', icon: Database },
      { title: 'voodu-redis', href: '/docs/plugins/redis', icon: Database },
      { title: 'voodu-caddy', href: '/docs/plugins/caddy', icon: Container },
      { title: 'Build your own', href: '/docs/plugins/build-your-own', icon: Plug },
    ],
  },
  {
    title: 'Reference',
    icon: BookOpen,
    list: [
      { title: 'FAQ', href: '/docs/reference/faq', icon: HelpCircle },
      { title: 'Security', href: '/docs/reference/security', icon: ShieldCheck },
      { title: 'Cron syntax', href: '/docs/reference/cron', icon: Clock },
      { title: 'Errors', href: '/docs/reference/errors', icon: AlertTriangle },
      { title: 'Contributing', href: '/docs/reference/contributing', icon: HandHelping },
    ],
  },
];
