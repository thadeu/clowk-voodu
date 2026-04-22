# Voodu — Plano de Execução

> **Status:** Pre-M0 (pronto pra começar)
> **Repo:** `thadeu/clowk-voodu`
> **Módulo Go:** `go.voodu.clowk.in`
> **Domínio install:** `clowk.in`
> **Binário:** `voodu` (canônico) + `vd` (alias)

---

## Contexto

Evolução do [Gokku](https://github.com/thadeu/gokku) com foco em **stateful services como cidadãos de primeira classe**. O Gokku já resolve deploy de apps via `git push` (battle-tested com 5 apps em prod). O Voodu mantém essa parte e investe onde o Gokku é fraco: **databases confiáveis** (Postgres, Mongo) com backup, replica, test-restore — tudo out-of-the-box, sem exigir N plugins separados como K8s.

**Bar pessoal:** roda meus apps, não perde dados, não me acorda 3am.

---

## Princípios de design

1. **Deployment já funciona no Gokku** — porta, não reinventa. Única mudança: Caddy no lugar de nginx.
2. **Postgres confiável é o produto** — se backup/restore não for sólido, nada mais importa.
3. **Escopo agressivamente mínimo** — se não serve os 5 apps em prod, espera.
4. **Plugin = qualquer executável** — bash, Go, Python, qualquer coisa. Contract via `plugin.yml`.
5. **Official plugins** (Caddy, Postgres, Mongo) em repos separados, opt-in via `voodu plugins:install`.
6. **HCL over YAML** — sem ceremony K8s-style (`kind/metadata/spec`). Block label = kind.
7. **Compor, não reinventar** — Docker SDK, Caddy, WAL-G, Patroni (no futuro).

---

## Arquitetura

```
voodu CLI (Cobra + dispatch dinâmico)
  │
  ├── git push → post-receive hook (Gokku-style, mantido)
  └── HTTP → voodu-controller
                │
                ├── etcd embedded (state, watch API)
                ├── Reconciler loop (watch-driven)
                ├── Plugin dispatcher (exec + JSON protocol)
                └── SSH → Docker SDK (nodes)
                        │
                      Containers (apps + databases)

Official plugins (opt-in, auto-install configurável):
  ├── voodu-caddy      (ingress + SSL automático)
  ├── voodu-postgres   (WAL-G backup + replica + test-restore)
  └── voodu-mongo      (mongodump + replica set)
```

---

## Repos

| Repo | Conteúdo |
|---|---|
| `thadeu/clowk-voodu` | CLI + controller + plugin SDK + deploy pipeline |
| `thadeu/voodu-caddy` | Plugin oficial Caddy |
| `thadeu/voodu-postgres` | Plugin oficial Postgres |
| `thadeu/voodu-mongo` | Plugin oficial Mongo |

Cada plugin versiona independente. Core pinna versões compatíveis em `compatible_plugins.yml`.

---

## Config format — HCL

Multi-resource em um arquivo (ou arquivos separados aplicados com `-f`):

```hcl
deployment "api" {
  replicas = 2

  container {
    image = "registry.io/api:${VERSION}"
    port  = 3000
    env = {
      APP_ENV = "production"
    }
    resources {
      cpu    = "500m"
      memory = "256mb"
    }
  }

  strategy {
    type      = "rolling"
    max_surge = 1
  }

  health_check {
    path      = "/health"
    interval  = "5s"
    threshold = 3
  }
}

database "main" {
  engine  = "postgres"
  version = "16"

  backup {
    destination = "r2://my-bucket/postgres"
    schedule    = "0 2 * * *"
    wal_archive = true
  }

  replica {
    count = 1
  }
}

ingress "api" {
  domain = "api.myapp.com"

  tls {
    provider = "letsencrypt"
    email    = "ops@myapp.com"
  }

  rule {
    path    = "/"
    service = "api"
  }
}
```

**Parsing:** `hashicorp/hcl/v2/hclsimple` — ~100 LOC de schema Go.

**Cross-refs (deferido pra v2):** começa literal + env interpolation. Refs tipo `database.main.url` viram função simples `database_url("main")` resolvida em apply-time.

---

## CLI surface

```bash
# Server
voodu server:add root@1.2.3.4
voodu server:list

# Plugins
voodu plugins:install caddy
voodu plugins:install postgres
voodu plugins:list

# Deploy (git push continua funcionando)
git push voodu main
voodu deploy                       # alternativa via CLI
voodu apply -f ./config/           # aplica HCL files
voodu status
voodu logs api
voodu rollback api
voodu scale api --replicas 5

# Config
voodu config:set DATABASE_URL=... -a api
voodu config:list -a api

# Database (via plugin)
voodu postgres:create main
voodu postgres:backup main
voodu postgres:restore main --from 2026-04-20T02:00:00
voodu postgres:replica:add main
voodu postgres:promote-replica main
voodu postgres:test-restore main
```

Sintaxe `cmd:sub` pré-processada pra cobra (`cmd sub`). `vd` é alias completo.

---

## Plugin contract

**Estrutura:**
```
/opt/voodu/plugins/<name>/
├── plugin.yml
├── bin/<entrypoint>      # qualquer executável
└── scripts/              # suporte
```

**`plugin.yml`:**
```yaml
name: postgres
version: "1.0.0"
min_voodu_version: "0.1.0"
entrypoint: bin/postgres

commands:
  create:
    description: "Create a postgres instance"
    args:
      - name: instance
        required: true
    flags:
      - name: version
        default: "16"
      - name: size
        default: "10gb"
  backup:
    description: "Backup to R2"
    args:
      - name: instance
        required: true
```

**Protocol:**
- Controller exec: `<entrypoint> <command> <args...>`
- Env vars injetados: `VOODU_APP`, `VOODU_STATE_DIR`, `VOODU_NODE`, `VOODU_LOG_LEVEL`, `VOODU_PLUGIN_DIR`
- Plugin responde via stdout JSON estruturado:
  ```json
  {"status": "ok", "data": {...}}
  {"status": "error", "error": "..."}
  ```
- CLI formata output (tabela, json, yaml via flag `-o`)

**Discovery:** CLI só conhece builtins. Comando não-builtin vai raw pro controller, que dispatcha pro plugin.

---

## State layout

**No servidor:**
```
/opt/voodu/
├── apps/<app>/{current,releases/<ts>,shared/.env,voodu.hcl}
├── services/<service>/
├── plugins/<name>/{plugin.yml,bin/,scripts/}
├── repos/<app>.git/
└── state/              # etcd embedded data
```

**Em etcd:**
```
/desired/deployments/<name>
/desired/databases/<name>
/desired/services/<name>
/desired/ingresses/<name>
/actual/nodes/<node>/containers/<id>
/actual/nodes/<node>/health
/config/<app>/<key>
/plugins/<name>/manifest
```

---

## Milestones

### M0 — Scaffolding (1 semana)

**Objetivo:** repo pronto pra commits de feature.

**Entregáveis:**
- [ ] Rename `go.mod` → `go.voodu.clowk.in`
- [ ] Substituir todos os imports `go.gokku-vm.com` → `go.voodu.clowk.in`
- [ ] Estrutura nova de dirs: `cmd/{cli,controller}`, `internal/`, `pkg/`, `examples/`
- [ ] Limpar/arquivar `v1/` e `pkg/` antigos (mover só o que vai portar)
- [ ] `.golangci.yml`, `.goreleaser.yml`, `Makefile` com targets `build/lint/test/install`
- [ ] GitHub Actions: CI + release automation
- [ ] `README.md` reescrito (mínimo)
- [ ] Deletar `gokku.yml`, `PLUGINS.md`, `RE_BRADING*.md` antigos (manter PLAN.md)

**Done quando:** `make build && make lint && make test` passa. `voodu --version` retorna `0.1.0-dev`.

---

### M1 — Port do Gokku (2 semanas)

**Objetivo:** paridade funcional com Gokku (deploy blue-green + config + SSH) no novo repo.

**Entregáveis:**
- [ ] `internal/lang/` portado de `pkg/lang/` (as-is)
- [ ] `internal/deploy/bluegreen.go` usando Docker SDK (não mais `exec.Command`)
- [ ] `internal/git/` — git bare + `post-receive` hook
- [ ] `internal/ssh/` — exec remoto + file copy
- [ ] `internal/config/secrets/` — `config:set|list|unset`
- [ ] Paths `/opt/voodu/`, env vars `VOODU_*`
- [ ] Plugin nginx (bash) portado temporariamente — será substituído por Caddy em M6

**Done quando:** `git push voodu main` faz deploy funcional de app Go e app Rails.

---

### M2 — CLI Cobra + dispatch (1 semana)

**Objetivo:** CLI completo com cobra, plugins dinâmicos no dispatcher.

**Entregáveis:**
- [ ] Cobra root command com pré-processador `:` → espaço
- [ ] Builtins: `deploy`, `apply`, `status`, `logs`, `exec`, `scale`, `config`, `server`, `plugins`, `version`
- [ ] Comando não-builtin → forward raw pro controller via HTTP
- [ ] Goreleaser pra darwin/linux × amd64/arm64
- [ ] Alias `vd` instalado via symlink na instalação

**Done quando:** `voodu --help` mostra árvore completa. `vd deploy` funciona. Comando desconhecido dá erro claro "plugin não encontrado".

---

### M3 — Controller daemon + etcd embed (2 semanas)

**Objetivo:** daemon rodando state-backed, reconciler watch-driven.

**Entregáveis:**
- [ ] `cmd/controller/` — daemon com `go.etcd.io/etcd/server/v3/embed`
- [ ] HTTP API: `/apply`, `/plugins`, `/status`, `/logs`, `/exec`
- [ ] Reconciler loop com etcd watch em `/desired/*`
- [ ] Systemd unit template
- [ ] Bootstrap: `voodu server:add` instala controller + inicia systemd unit

**Done quando:** `systemctl start voodu-controller` sobe. CLI POST `/apply` persiste em etcd. Restart mantém state.

---

### M4 — HCL config + apply -f (1 semana)

**Objetivo:** parser HCL dos 4 kinds + ergonomia kubectl-style.

**Entregáveis:**
- [ ] Schema HCL via `hclsimple` pros kinds: `deployment`, `database`, `service`, `ingress`
- [ ] `voodu apply -f <file>` — arquivo único
- [ ] `voodu apply -f <dir>` — scan recursivo `.hcl`
- [ ] `voodu apply -f file1.hcl -f file2.hcl` — múltiplos `-f`
- [ ] `voodu apply -f -` — stdin
- [ ] `voodu diff -f <file>` — mostra mudanças pré-apply
- [ ] `voodu delete -f <file>` — remove recursos declarados
- [ ] Interpolação de env vars (`${VERSION}`)
- [ ] `examples/` cobrindo cada kind

**Done quando:** `voodu apply -f ./examples/fullstack/` valida 3 arquivos `.hcl` separados (deployment, database, ingress) e escreve em etcd (mesmo que plugins de database/ingress ainda não existam — deployment sobe).

---

### M5 — Plugin system (2 semanas)

**Objetivo:** plugin SDK + dispatcher + `plugins:install`.

**Entregáveis:**
- [ ] Spec `plugin.yml` documentada em `pkg/plugin/SPEC.md`
- [ ] `internal/plugins/loader.go` — lê manifests, valida schema
- [ ] `internal/plugins/exec.go` — exec + env vars padrão + JSON parse
- [ ] JSON output schema strict (status/data/error), CLI formata com `-o table|json|yaml`
- [ ] `voodu plugins:install <source>` — git repo, URL tarball, local path
- [ ] `voodu plugins:list|remove|update`
- [ ] Plugin SDK em `pkg/plugin/` (helpers Go pra plugin authors)
- [ ] Plugin de teste `voodu-hello` pra validar o contrato

**Done quando:** `voodu plugins:install github.com/thadeu/voodu-hello && voodu hello say oi` retorna JSON formatado pelo CLI.

---

### M6 — Plugin oficial `voodu-caddy` (1-2 semanas)

**Repo novo:** `thadeu/voodu-caddy`

**Entregáveis:**
- [ ] Plugin Go compilado linux/amd64 + linux/arm64
- [ ] Installer baixa binário Caddy + configura systemd
- [ ] Comandos: `voodu ingress:add|remove|list|reload`
- [ ] Integração com `kind: ingress` do HCL
- [ ] Caddy Admin API client (config dinâmica sem reload)
- [ ] Auto-SSL Let's Encrypt

**Done quando:** `voodu plugins:install caddy && voodu apply -f ingress.hcl` resulta em domínio HTTPS acessível em < 60s.

---

### M7 — Plugin oficial `voodu-postgres` — O PRODUTO (4 semanas)

**Repo novo:** `thadeu/voodu-postgres`

**M7.1 — Provision (1 semana)**
- [ ] `voodu postgres:create <name>` + `kind: database` no HCL
- [ ] Container + volume nomeado + tuning básico
- [ ] Injection de `DATABASE_URL` no `.env` do app linkado
- [ ] `voodu postgres:list|info|destroy`

**M7.2 — Backup WAL-G (1 semana)**
- [ ] WAL archive contínuo → R2
- [ ] Daily base backup → R2
- [ ] Config de retention
- [ ] `voodu postgres:backup:list|now`
- [ ] Log estruturado "last backup: ok, N minutes ago"

**M7.3 — Restore + test-restore (1 semana)**
- [ ] `voodu postgres:restore <name> --from <ts>`
- [ ] `voodu postgres:test-restore <name>` — efêmero, valida, derruba
- [ ] Cron weekly pra test-restore automático
- [ ] Alarm (exit != 0, log level error) se falhar

**M7.4 — Replica + promote manual (1 semana)**
- [ ] `voodu postgres:replica:add <name>`
- [ ] Streaming replication via `pg_basebackup`
- [ ] `voodu postgres:replica:status`
- [ ] `voodu postgres:promote-replica <name>` (manual)

**Done quando o teste de fogo passa:**
1. Cria Postgres
2. Injeta dados
3. Espera backup automático rodar
4. Destrói container + volume
5. Restore recupera 100% dos dados
6. Promove replica em < 2min

Se passar, **vale migrar o primeiro app Gokku.**

---

### M8 — Migration primeiro app Gokku (1 semana)

**Objetivo:** primeiro app em prod rodando no Voodu.

**Entregáveis:**
- [ ] `scripts/migrate-from-gokku.sh` — lê `/opt/gokku/`, escreve `/opt/voodu/`
- [ ] Tradução `gokku.yml` → `voodu.hcl` multi-resource
- [ ] Safety net: dump paralelo pra Neon durante observação
- [ ] Rollback plan documentado em `docs/migration.md`

**Done quando:** app menor dos 5 roda em Voodu por 7 dias sem incidente, test-restore passa toda semana.

---

### M9 — Plugin oficial `voodu-mongo` (2 semanas)

**Repo novo:** `thadeu/voodu-mongo`

Paridade com postgres: provision, backup (`mongodump` → R2), restore, replica set, promote.

---

### M10+ — Backlog

- Patroni integration (failover automático) — só quando M7 tiver 6+ meses estável
- Rollout dos 4 apps restantes (1/semana)
- Redis plugin oficial — sob demanda
- Web UI — só se fizer sentido
- Multi-region

---

## Timeline

```
M0  ███                                              1w
M1  ██████████                                       2w
M2    █████                                          1w
M3        ██████████                                 2w
M4            █████                                  1w
M5              ██████████                           2w
M6                  ██████████                       1-2w
M7                      ███████████████████████      4w  ← O PRODUTO
M8                                              ███  1w  ← prova real
─────────────────────────────────────────────────────
                              Total M0-M8: ~16 semanas (~4 meses)
```

---

## Paralelismo possível

- M1 e M2 podem andar juntas (port do Gokku ≠ CLI cobra, não se tocam muito)
- M6 (Caddy) pode começar quando M5 estiver 50% (SDK pronto)
- M7 (Postgres) pode começar em paralelo com M6 — repos diferentes

---

## Não-negociáveis

1. **Postgres com backup/restore sólido** — M7 completo antes de migrar qualquer app
2. **Safety net** — dump pra Neon/Supabase durante período de confiança
3. **Test-restore semanal** — se não passa, alarme dispara, não importa o que seja
4. **git push mantido** — não quebrar o UX que já funciona

---

## Pendências de branding (não bloqueiam código)

- GitHub org — `thadeu/clowk-voodu` por enquanto, criar `clowk-dev` depois se fizer sentido

---

## Next action

Executar **M0 — Scaffolding**.
