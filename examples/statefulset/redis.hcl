// Redis as a statefulset with persistent AOF on disk. Same
// shape as the postgres example: single ordinal, one
// volume_claim mounted at /data, hardened command line.
//
// To customise extensively (maxmemory, ACL, TLS), add an
// `asset` block with redis.conf and reference it in `volumes`
// — see ../stack for the full pattern.

statefulset "data" "cache" {
  image    = "redis:7-alpine"
  replicas = 1

  // The default redis-server command turns AOF on so data
  // survives restarts. Operator who needs ACL / TLS / custom
  // tuning replaces this with a config-file invocation:
  //
  //   command = ["redis-server", "/etc/redis/redis.conf"]
  //
  // (paired with a redis.conf mounted via an `asset` block).
  command = ["redis-server", "--appendonly", "yes"]

  ports = ["6379"]

  volume_claim "data" {
    mount_path = "/data"
  }
}
