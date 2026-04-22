# Remote Setup

Learn how `gokku remote setup` provisions a new server and prepares it for deployments without manual SSH sessions.

## Overview

`gokku remote setup` is a one-time bootstrap command that connects to your server, installs the required components, and creates a default Git remote on your local repository. Run it from your development machine once per server.

```bash
gokku remote setup user@server_ip
```

Replace `user@server_ip` with the SSH user and address of your host (public IP or DNS).

## Prerequisites

- You have the Gokku CLI installed locally (`gokku version`).
- Your project is a Git repository (the command adds a remote).
- The remote host runs a modern 64-bit Linux distribution (Ubuntu 22.04 or later is recommended).
- The SSH user has sudo privileges without interactive prompts.
- Port 22 is reachable from your machine.
- You can SSH into the server without typing a password, or you have a PEM key file ready.

If you need to copy your SSH public key to the server beforehand, use:

```bash
ssh-copy-id user@server_ip
```

## Running the setup

Execute the command from the root of your application repository:

```bash
gokku remote setup ubuntu@ec2-1-2-3-4.compute.amazonaws.com
```

By default, the command uses your standard SSH configuration. To supply a specific identity file, use `-i` or `--identity`:

```bash
gokku remote setup ubuntu@ec2.example.com -i ~/.ssh/my-key.pem
```

The identity file must exist locally before running the command. Gokku forwards it to every SSH call during the setup.

## What the command does

Each execution performs the following phases:

1. **Prerequisite check** - Confirms SSH connectivity in batch mode so no password prompts appear.
2. **Gokku installation** - Runs the remote installation script (`curl ... | bash -- --server`) which installs Docker, Gokku binaries, systemd units, and supporting scripts.
3. **Plugin installation** - Installs the essential plugins: `nginx`, `letsencrypt`, `cron`, `postgres`, and `redis`. Existing plugins are skipped.
4. **SSH key propagation** - If you are not using an identity file, Gokku copies your local public key to the remote `~/.ssh/authorized_keys`.
5. **Verification** - Checks Docker availability, plugin directories, and the base `/opt/gokku` structure.
6. **Default remote creation** - Adds a local Git remote named `gokku` pointing to the server. If the repository already has that remote or Git is not initialized, the step is skipped with a helpful message.

Console output follows the standard Heroku-style log format, so you see a step-by-step progress stream, including warnings if a plugin cannot be provisioned.

## After setup

Once the command finishes:

- Run `gokku remote list` to confirm the `gokku` remote exists.
- Create an app on the server with `gokku apps create my-service --remote` or rely on auto-creation during the first deploy.
- Add additional Git remotes for each app: `gokku remote add api-production user@server_ip`.
- Perform the first deployment with `git push api-production main`.

## Troubleshooting

**SSH connection failed**  
The command exits early if the server refuses the connection or requires a password. Confirm that you can run `ssh user@server_ip "echo ok"` from the same machine. Use `ssh-copy-id` or provide the correct identity file.

**Installer aborts with missing dependencies**  
Log into the server and ensure `curl`, `git`, and `sudo` are installed. The remote script will attempt to install the remaining packages automatically, but missing basics prevent it from running.

**Plugin installation warnings**  
If you see warnings while installing plugins, rerun the setup once the underlying issue is fixed, or install the plugin manually with `gokku plugins:add <name>` after logging into the server.

**Git remote not created**  
Make sure you executed the command inside a Git repository. You can add the remote manually later with `gokku remote add gokku user@server_ip`.

The setup is idempotent. You can rerun it at any time to verify the server or reinstall missing components. Gokku skips resources that already exist.

