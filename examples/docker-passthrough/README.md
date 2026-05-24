# docker-passthrough

Raw `docker run` pass-throughs for shapes the typed surface doesn't model.

## `ulimits = {}`

A map of per-resource ulimit names to values. Each entry produces one `--ulimit <key>=<value>` flag at container create. Voodu's platform defaults (`nofile=65536:65536`, `nproc=4096:4096`) still apply for any key the operator does NOT declare — overrides are per-key, not whole-table.

```hcl
ulimits = {
  nofile  = "1048576:1048576"   # huge file table (overrides default)
  memlock = "-1"                 # unbounded locked memory (new key)
  // nproc not declared → keeps platform default 4096:4096
}
```

Values flow verbatim: docker accepts both `"N"` (soft=hard) and `"soft:hard"` shapes. No name validation — a typo lands as a docker daemon error at container create, not at apply.

## `docker_options = []`

A list of raw flag strings appended verbatim to `docker run` between voodu's managed flags and the image. No parsing, no validation. Use for compose-only knobs voodu doesn't model.

```hcl
docker_options = [
  "--shm-size=2g",
  "--sysctl=net.core.somaxconn=4096",
  "--pids-limit=4096",
  "--device=/dev/snd",
  "--privileged",   // if you really need it
]
```

## Footgun

Do NOT redeclare flags voodu already manages — docker rejects duplicates at create time:

| flag voodu manages | how to influence it |
|---|---|
| `--name` | (not configurable — derived from scope/name) |
| `--network` | `networks = [...]` / `network_mode = "..."` |
| `--restart` | `restart = "..."` |
| `--env-file` | `env_file = [...]` / `env_from = [...]` |
| `--cpus`, `--memory` | `resources { limits { cpu, memory } }` |
| `--ulimit` | `ulimits = {...}` (this field) |
| `--label` | (reserved for voodu identity) |
| `--add-host` | `extra_hosts = [...]` |
| `--cap-add` | `cap_add = [...]` |
| `--log-opt` | `logs { max_size, max_files }` |

## Where it applies

Every container-spawning kind: `deployment`, `statefulset`, `job`, `cronjob`, `app`, and per-init under `init {}`. Plugin kinds (`postgres`, `redis`, `mongo`, `caddy`, …) accept the fields at the HCL surface (plugin blocks are schema-free), and any plugin that forwards them into the emitted deployment/statefulset wire spec will get the runtime behaviour. If a plugin you depend on doesn't pass them through yet, drop down to a plain `statefulset {}` and set the fields there.

## Hash semantics

Both fields fold into the spec hash on deployments and statefulsets — editing either triggers a rolling restart so the new flags take effect. Without this, docker would freeze the flags at create time and live replicas would keep the old runtime parameters silently. Jobs and cronjobs read the spec fresh on every run, so no hash needed.

## Inside `init {}`

Per-init `ulimits` / `docker_options` REPLACE the parent's at the init's docker-run — they don't merge per-key. Useful when a one-off prep step (heavy schema dump, large index build) needs different limits from the steady-state pod.
