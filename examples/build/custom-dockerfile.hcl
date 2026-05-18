// docker-compose-shaped: custom Dockerfile + build args + Linux
// capabilities + extra /etc/hosts entries. This is the "freeswitch /
// proprietary daemon" use case — non-trivial network and kernel
// requirements that need a hand-written Dockerfile.
//
// Why each knob:
//
//   - `build.context`     — apps/esl has the entire service tree (configs,
//                            scripts, the Dockerfile). Tarballed and sent
//                            to docker build.
//   - `build.dockerfile`  — custom name; voodu's auto-gen path is skipped
//                            because this file ships with the repo.
//   - `build.args`        — parametrises one Dockerfile for multiple
//                            services (SERVICE=api, SERVICE=worker, …).
//                            docker-compose's `build.args` 1:1.
//   - `cap_add SYS_NICE`  — FreeSWITCH wants realtime scheduling
//                            (RT priority). Without CAP_SYS_NICE the
//                            kernel rejects sched_setscheduler() calls.
//   - `extra_hosts`       — internal service that doesn't have docker
//                            DNS (legacy SIP gateway on a fixed IP).
//
// Note: no `lang {}` block. Custom Dockerfiles bypass language
// handlers — voodu just calls `docker build` with the file the
// operator provided.

deployment "voice" "fsw" {
  replicas = 1
  ports    = ["5060/udp", "5061/tcp", "16384-32768/udp"]

  // freeswitch needs the host network for RTP NAT traversal —
  // declaring network_mode = "host" disables docker bridge and the
  // container sees the host's interfaces directly.
  network_mode = "host"

  cap_add = ["SYS_NICE", "NET_ADMIN"]

  extra_hosts = [
    "sip-gateway:10.0.0.42",
    "voicemail-store:10.0.0.43",
  ]

  env = {
    FS_PROFILE = "production"
  }

  build {
    context    = "apps/esl"
    dockerfile = "Dockerfile.fsw"

    args = {
      SERVICE       = "fsw"
      FREESWITCH_TAG = "1.10.11"
    }
  }
}
