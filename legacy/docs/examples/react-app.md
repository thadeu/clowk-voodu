# React Application

Deploy a React application with Node.js backend using gokku.yml configuration.

## Basic Setup

### Project Structure

```
my-react-app/
├── client/          # React frontend
│   ├── public/
│   ├── src/
│   ├── package.json
│   └── build/
├── server/          # Node.js API backend
│   ├── src/
│   ├── package.json
│   └── index.js
├── Dockerfile
└── gokku.yml
```

### gokku.yml

```yaml
apps:
  react-app:
    lang: nodejs
    path: .
    deployment:
      post_deploy:
        - npm run db:migrate
        - npm run cache:warm
```

## Post-Deploy Commands

The `post_deploy` commands run automatically after successful deployment:

- **Database migrations**: `npm run db:migrate` (for schema updates)
- **Cache warming**: `npm run cache:warm` (pre-populate application caches)
- **Data seeding**: Populate initial data if needed
- **Custom tasks**: Any command needed after deployment

**Commands run in sequence** and **fail deployment** if any command fails. This ensures your app is fully ready before considering the deployment complete.

## Deployment

### 1. Initialize Git

```bash
cd my-react-app
git init
git add .
git commit -m "Initial commit"
```

### 2. Setup Remote

```bash
# Add remote (replace with your server)
gokku remote add react-app-production ubuntu@your-server
```

### 3. Deploy

```bash
git push react-app-production main
```

Gokku will automatically:
- Use your Dockerfile or generate one for Node.js
- Build the React app
- Deploy the application
- **Run post-deploy commands** (database migrations, cache warming)

### 4. Check Status

```bash
# Check application status
gokku status react-app production -a react-app-production

# View logs
gokku logs react-app production -f -a react-app-production
```

## Environment Variables

### Database Configuration

```bash
# Set database URL
gokku config set DATABASE_URL="mongodb://localhost:27017/react_app_production" -a react-app-production

# Set Redis URL
gokku config set REDIS_URL="redis://localhost:6379" -a react-app-production
```

### Application Configuration

```bash
# Set JWT secret
gokku config set JWT_SECRET="your-jwt-secret-here" -a react-app-production

# Set environment
gokku config set NODE_ENV=production -a react-app-production

# Set API port
gokku config set API_PORT=3001 -a react-app-production
```

### API Keys and External Services

```bash
# Set API keys
gokku config set STRIPE_SECRET_KEY="sk_live_..." -a react-app-production
gokku config set AWS_ACCESS_KEY_ID="..." -a react-app-production

# Set external service URLs
gokku config set EXTERNAL_API_URL="https://api.example.com" -a react-app-production
```

## Monitoring

### Application Status

```bash
# Check application status
gokku status react-app production -a react-app-production

# Output:
# === Application Status: react-app (production) ===
#
# Container: running
# Health: healthy
# Port: 3000
```

### Application Logs

```bash
# Application logs
gokku logs react-app production -f -a react-app-production
```

## Next Steps

- [Environment Variables](/guide/environments) - Environment management
- [Docker Support](/guide/docker) - Container deployment
- [Configuration](/reference/configuration) - Advanced configuration
