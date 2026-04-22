# voodu

> PaaS self-hosted, git-push style, com stateful services de primeira classe.

**Status:** em desenvolvimento ativo — fase **M2** (CLI Cobra com dispatch de plugins).

Evolução do [Gokku](https://github.com/thadeu/gokku) mantendo o que funciona (deploy via `git push`, blue-green, `config:set`) e investindo onde o Gokku é fraco: Postgres/Mongo com backup, replica e test-restore built-in, sem exigir N plugins separados como Kubernetes.

## Roadmap

Plano de execução detalhado em [PLAN.md](PLAN.md). Resumo:

```
M0  Scaffolding                ✓
M1  Port do Gokku              ✓
M2  CLI Cobra + dispatch       ← você está aqui
M3  Controller + etcd embed
M4  HCL config + apply -f
M5  Plugin system
M6  Plugin oficial Caddy
M7  Plugin oficial Postgres (o produto)
M8  Migração primeiro app
```

### M1 — o que já funciona

- `voodu setup` — prepara `/opt/voodu` no servidor
- `voodu apps create <name>` — cria diretórios, bare repo e post-receive hook
- `voodu apps list`
- `voodu deploy -a <app>` — pipeline completo (extract → build → swap → post-deploy → restart)
- `voodu config set|get|list|unset|reload -a <app>` — gerencia `.env`
- Exemplo de configuração em [examples/voodu.yml](examples/voodu.yml)
- Pacotes internos portados de Gokku: `config`, `envfile`, `paths`, `util`, `docker`, `containers`, `lang` (go/python/nodejs/ruby/rails/generic), `git`, `ssh`, `secrets`, `deploy`.

### M2 — o que já funciona

- **Cobra completo**: árvore de comandos com `apply`, `status`, `logs`, `exec`, `scale`, `server`, `plugins`, `version` (stubs claros apontam o milestone em que cada um lança)
- **Sintaxe `cmd:sub`**: `voodu config:set FOO=bar -a api` funciona igual a `voodu config set FOO=bar -a api`; urls e remotes `user@host:app` **não** são quebrados
- **Dispatch de plugins**: comando desconhecido é forwarded pro controller via HTTP `POST /exec`. Sem controller rodando, mensagem clara aponta que o daemon chega no M3 e plugins no M5
- **`--controller-url` / `VOODU_CONTROLLER_URL`**: endpoint configurável
- **Protocolo JSON** (`{"status":"ok","data":...}` / `{"status":"error","error":"..."}`) já honrado pelo CLI — plugins shell que emitem JSON já funcionam; plain stdout também passa

## Install (quando estável)

```bash
curl -fsSL https://clowk.in/install | bash
```

## Development

```bash
make tidy          # download deps
make build         # build voodu + voodu-controller
make check         # fmt + vet + lint + test
./bin/voodu --version
```

## Repos relacionados

| Repo | Conteúdo |
|---|---|
| `thadeu/clowk-voodu` | Core (CLI + controller) — **este repo** |
| `thadeu/voodu-caddy` | Plugin oficial Caddy (ingress + SSL) |
| `thadeu/voodu-postgres` | Plugin oficial Postgres |
| `thadeu/voodu-mongo` | Plugin oficial Mongo |

## License

MIT — ver [LICENSE](LICENSE).
