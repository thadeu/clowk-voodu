database "main" {
  engine  = "postgres"
  version = "16"
  storage = "20Gi"
  replicas = 1

  backup {
    schedule  = "0 3 * * *"
    retention = "14d"
    target    = "s3://clowk-backups/voodu/main"
  }

  params = {
    max_connections = "200"
    shared_buffers  = "512MB"
  }
}
