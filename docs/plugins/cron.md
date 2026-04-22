# Cron Plugin

The Cron plugin provides scheduled task management with job scheduling, logging, and monitoring capabilities for your Gokku applications.

## Installation

```bash
gokku plugins:add cron
```

## Features

- **Job Scheduling**: Schedule tasks using cron syntax
- **Job Management**: Create, list, and remove scheduled jobs
- **Logging**: Comprehensive job execution logging
- **Monitoring**: Job status and execution monitoring
- **Error Handling**: Automatic error handling and notifications
- **Multiple Environments**: Support for different environments

## Usage

### Schedule Jobs

```bash
# Schedule a job with cron syntax
gokku cron:schedule "0 2 * * *" "backup-database.sh"

# Schedule job with specific name
gokku cron:schedule "0 2 * * *" "backup-database.sh" --name "daily-backup"

# Schedule job for specific app
gokku cron:schedule "0 2 * * *" "backup-database.sh" -a api-production
```

### Job Management

```bash
# List all scheduled jobs
gokku cron:list

# Show job information
gokku cron:info daily-backup

# Remove a job
gokku cron:remove daily-backup

# Run a job manually
gokku cron:run daily-backup
```

### Job Logs

```bash
# View job logs
gokku cron:logs daily-backup

# View logs for specific job
gokku cron:logs-job daily-backup

# Follow logs in real-time
gokku cron:logs daily-backup -f
```

### Service Management

```bash
# Show service information
gokku cron:info

# View service logs
gokku cron:logs

# Restart service
gokku cron:restart
```

## Cron Syntax

The cron plugin uses standard cron syntax:

```
* * * * *
│ │ │ │ │
│ │ │ │ └─── Day of week (0-7, 0 and 7 are Sunday)
│ │ │ └───── Month (1-12)
│ │ └─────── Day of month (1-31)
│ └───────── Hour (0-23)
└─────────── Minute (0-59)
```

### Common Examples

```bash
# Every minute
gokku cron:schedule "* * * * *" "check-health.sh"

# Every hour
gokku cron:schedule "0 * * * *" "cleanup-temp.sh"

# Every day at 2 AM
gokku cron:schedule "0 2 * * *" "backup-database.sh"

# Every Monday at 3 AM
gokku cron:schedule "0 3 * * 1" "weekly-report.sh"

# Every 15 minutes
gokku cron:schedule "*/15 * * * *" "monitor-system.sh"

# Every weekday at 9 AM
gokku cron:schedule "0 9 * * 1-5" "daily-sync.sh"
```

## Job Types

### Shell Scripts

```bash
# Simple shell script
gokku cron:schedule "0 2 * * *" "backup.sh"

# Script with arguments
gokku cron:schedule "0 2 * * *" "backup.sh --full --compress"
```

### Application Commands

```bash
# Run application command
gokku cron:schedule "0 2 * * *" "gokku run 'rails runner \"User.cleanup\"' -a api-production"

# Run database migration
gokku cron:schedule "0 3 * * *" "gokku run 'rails db:migrate' -a api-production"
```

### Docker Commands

```bash
# Run Docker command
gokku cron:schedule "0 4 * * *" "docker system prune -f"

# Run specific container command
gokku cron:schedule "0 5 * * *" "docker exec api-production rails runner \"Report.generate\""
```

## Configuration

### Environment Variables

Set cron-specific environment variables:

```bash
# Set timezone
gokku config set CRON_TIMEZONE=UTC -a cron

# Set log level
gokku config set CRON_LOG_LEVEL=INFO -a cron

# Set job timeout
gokku config set CRON_JOB_TIMEOUT=3600 -a cron

# Set max concurrent jobs
gokku config set CRON_MAX_JOBS=10 -a cron
```

### Job Configuration

```bash
# Set job environment variables
gokku config set JOB_ENV=production -a cron

# Set job working directory
gokku config set JOB_WORK_DIR=/opt/gokku/jobs -a cron

# Set job log directory
gokku config set JOB_LOG_DIR=/opt/gokku/logs/cron -a cron
```

## Monitoring

### Job Status

```bash
# Check job status
gokku cron:info daily-backup

# List all jobs with status
gokku cron:list

# Check service status
gokku cron:status
```

### Logs

```bash
# View job logs
gokku cron:logs daily-backup

# View service logs
gokku cron:logs

# View logs for specific time range
gokku cron:logs daily-backup --since "2024-01-01" --until "2024-01-02"
```

## Common Use Cases

### Database Backups

```bash
# Daily database backup
gokku cron:schedule "0 2 * * *" "gokku postgres:backup db-primary > /backups/db-\$(date +%Y%m%d).sql"

# Weekly full backup
gokku cron:schedule "0 3 * * 0" "gokku postgres:backup db-primary > /backups/db-weekly-\$(date +%Y%m%d).sql"
```

### System Maintenance

```bash
# Clean up old logs
gokku cron:schedule "0 1 * * *" "find /opt/gokku/logs -name '*.log' -mtime +30 -delete"

# Clean up Docker images
gokku cron:schedule "0 4 * * *" "docker system prune -f"

# Update system packages
gokku cron:schedule "0 5 * * 0" "apt update && apt upgrade -y"
```

### Application Tasks

```bash
# Generate daily reports
gokku cron:schedule "0 6 * * *" "gokku run 'rails runner \"Report.generate_daily\"' -a api-production"

# Send email notifications
gokku cron:schedule "0 9 * * *" "gokku run 'rails runner \"Notification.send_daily\"' -a api-production"

# Process queued jobs
gokku cron:schedule "*/5 * * * *" "gokku run 'rails runner \"Job.process_queue\"' -a api-production"
```

### Health Checks

```bash
# Check application health
gokku cron:schedule "*/5 * * * *" "curl -f http://localhost:8080/health || echo 'Health check failed'"

# Check database connectivity
gokku cron:schedule "*/10 * * * *" "gokku postgres:psql db-primary -c 'SELECT 1;'"
```

## Troubleshooting

### Job Not Running

```bash
# Check job status
gokku cron:info job-name

# Check service logs
gokku cron:logs

# Check job logs
gokku cron:logs job-name
```

### Common Issues

1. **Job not scheduled**: Check cron syntax and service status
2. **Permission denied**: Ensure scripts are executable
3. **Path issues**: Use absolute paths in job commands
4. **Environment variables**: Check if required env vars are set

### Debug Commands

```bash
# Test cron syntax
gokku cron:test "0 2 * * *"

# Check service status
gokku cron:status

# View service configuration
gokku cron:config
```

## Best Practices

1. **Use descriptive names**: Give jobs meaningful names
2. **Test before scheduling**: Test jobs manually before scheduling
3. **Monitor logs**: Regularly check job execution logs
4. **Handle errors**: Implement proper error handling in scripts
5. **Use absolute paths**: Always use absolute paths in job commands
6. **Set timeouts**: Set appropriate timeouts for long-running jobs

## Examples

### Complete Setup

```bash
# 1. Install plugin
gokku plugins:add cron

# 2. Create cron service
gokku services:create cron --name cron-service

# 3. Schedule database backup
gokku cron:schedule "0 2 * * *" "gokku postgres:backup db-primary > /backups/db-\$(date +%Y%m%d).sql" --name "daily-backup"

# 4. Schedule log cleanup
gokku cron:schedule "0 1 * * *" "find /opt/gokku/logs -name '*.log' -mtime +30 -delete" --name "log-cleanup"

# 5. Schedule health check
gokku cron:schedule "*/5 * * * *" "curl -f http://localhost:8080/health || echo 'Health check failed'" --name "health-check"

# 6. List all jobs
gokku cron:list
```

### Multiple Environments

```bash
# Production jobs
gokku cron:schedule "0 2 * * *" "backup-prod.sh" -a api-production

# Staging jobs
gokku cron:schedule "0 3 * * *" "backup-staging.sh" -a api-staging

# Development jobs
gokku cron:schedule "0 4 * * *" "backup-dev.sh" -a api-development
```

## Security

- **Access Control**: Jobs run with appropriate permissions
- **Logging**: All job executions are logged
- **Error Handling**: Failed jobs are properly logged
- **Resource Limits**: Jobs have timeout and resource limits

## Next Steps

- [PostgreSQL Plugin](/guide/plugins-postgres) - Add relational database
- [Redis Plugin](/guide/plugins-redis) - Add caching layer
- [Nginx Plugin](/guide/plugins-nginx) - Add load balancer
- [Environment Variables](/guide/env-vars) - Configure your app
