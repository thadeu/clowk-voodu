// 3-replica postgres skeleton showing how the per-pod aliases
// land. The bare statefulset DOES NOT bootstrap replication on
// its own — pod-1 and pod-2 will start as independent
// primaries unless an entrypoint script wires them up. This
// example is here to show the IDENTITY shape (DNS, volumes,
// labels), not a production-ready cluster.
//
// Real multi-node postgres needs:
//   - pod-0: regular initdb
//   - pod-1+: pg_basebackup against pg-0.data, then start as
//     hot standby
// That logic lives in a custom image (extending
// `postgres:15-alpine` with an entrypoint that branches on
// HOSTNAME or VOODU_REPLICA_ORDINAL) or in the future
// voodu-postgres-cluster plugin.
//
// What this manifest DOES give you:
//
//   3 docker containers:
//     data-pg.0   (DNS: pg-0.data, pg.data — primary by convention)
//     data-pg.1   (DNS: pg-1.data, pg.data)
//     data-pg.2   (DNS: pg-2.data, pg.data)
//
//   3 docker volumes (each pod's own state):
//     voodu-data-pg-data-0
//     voodu-data-pg-data-1
//     voodu-data-pg-data-2
//
//   The shared alias (`pg.data`) round-robins between all
//   three on docker DNS — useful for read-only clients that
//   tolerate connecting to any replica. The per-pod aliases
//   give deterministic targeting (`pg-0.data` always = pod-0).

statefulset "data" "pg" {
  image    = "postgres:15-alpine"
  replicas = 3

  env = {
    POSTGRES_DB = "myapp"
    PGDATA      = "/var/lib/postgresql/data/pgdata"
  }

  ports = ["5432"]

  volume_claim "data" {
    mount_path = "/var/lib/postgresql/data"
  }
}
