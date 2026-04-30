// asset — declarative file bundles materialised onto the host
// filesystem so deployments / statefulsets can mount them as
// bind volumes via ${asset.<name>.<key>} interpolation.
//
// The body is a flat (key → source) map. Each key becomes a
// filename under /opt/voodu/assets/<scope>/<name>/<key> on the
// server. The key is just an identifier (alphanumeric +
// underscore + hyphen, NO dots) — the real on-disk filename
// the container sees is set by the mount target in `volumes`.
//
// Three source kinds are accepted:
//
//   file("path")    — read locally at `vd apply` time, bytes
//                     embedded in the manifest. Path is
//                     relative to the CLI's CWD.
//   url("https://…") — fetched server-side at reconcile time,
//                     cached by ETag/Last-Modified under
//                     /opt/voodu/cache/. Pre-signed URLs
//                     (S3 / R2) are the recommended way to
//                     ship private bytes.
//   "literal string"— inline content, embedded in the manifest
//                     verbatim. Useful for tiny snippets
//                     where a separate file would be overkill.

asset "data" "redis-config" {
  // Local file read at apply time on the operator's machine.
  configuration = file("./redis/redis.conf")

  // Fetched by the controller; cached on subsequent applies.
  // Use a pre-signed URL for private buckets.
  users_acl = url("https://r2.example.com/configs/redis-users.acl")

  // Inline string — written verbatim. Container sees it as
  // a plain text file at the mount target you choose.
  motd = "Welcome to production redis"
}
