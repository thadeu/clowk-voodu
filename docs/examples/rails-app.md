# Rails Application

Deploy a Ruby on Rails application using gokku.yml configuration.

## Basic Setup

### Project Structure

```
my-rails-app/
├── app/
├── config/
├── db/
├── Gemfile
├── Gemfile.lock
├── Dockerfile
└── gokku.yml
```

### gokku.yml

```yaml
apps:
  rails-app:
    lang: ruby
    path: .
    deployment:
      post_deploy:
        - bundle exec rails db:migrate
        - bundle exec rails assets:precompile
```

## Post-Deploy Commands

The `post_deploy` commands run automatically after successful deployment:

- **Database migrations**: `bundle exec rails db:migrate`
- **Asset compilation**: `bundle exec rails assets:precompile`
- **Cache warming**: Pre-populate caches for faster responses
- **Custom tasks**: Any command needed after deployment

**Commands run in sequence** and **fail deployment** if any command fails. This ensures your app is fully ready before considering the deployment complete.

## Deployment

### 1. Initialize Git

```bash
cd my-rails-app
git init
git add .
git commit -m "Initial commit"
```

### 2. Setup Remote

```bash
# Add remote (replace with your server)
git remote add production ubuntu@your-server:rails-app
```

### 3. Deploy

```bash
git push production main
```

Gokku will automatically:
- Use your Dockerfile or generate one for Ruby
- Build the container
- Deploy the application
- **Run post-deploy commands** (database migrations, asset compilation)

### 4. Check Status

```bash
# Check application status
gokku status rails-app production -a rails-app-production

# View logs
gokku logs rails-app production -f -a rails-app-production
```

## Environment Variables

### Database Configuration

```bash
# Set database URL
gokku config set DATABASE_URL="postgres://user:password@localhost:5432/rails_app_production" -a rails-app-production

# Set Redis URL for Sidekiq
gokku config set REDIS_URL="redis://localhost:6379" -a rails-app-production
```

### Rails Configuration

```bash
# Set Rails secret key base
gokku config set SECRET_KEY_BASE="$(bundle exec rails secret)" -a rails-app-production

# Set environment
gokku config set RAILS_ENV=production -a rails-app-production

# Set log level
gokku config set RAILS_LOG_LEVEL=info -a rails-app-production
```

### Application-Specific Variables

```bash
# Set SMTP configuration
gokku config set SMTP_HOST="smtp.gmail.com" -a rails-app-production
gokku config set SMTP_PORT="587" -a rails-app-production

# Set API keys
gokku config set STRIPE_SECRET_KEY="sk_live_..." -a rails-app-production
gokku config set AWS_ACCESS_KEY_ID="..." -a rails-app-production
```

## Monitoring

### Application Status

```bash
# Check application status
gokku status rails-app production -a rails-app-production

# Output:
# === Application Status: rails-app (production) ===
#
# Container: running
# Health: healthy
# Port: 3000
```

### Application Logs

```bash
# Rails application logs
gokku logs rails-app production -f -a rails-app-production
```

## Next Steps

- [Environment Variables](/guide/environments) - Environment management
- [Docker Support](/guide/docker) - Container deployment
- [Configuration](/reference/configuration) - Advanced configuration
