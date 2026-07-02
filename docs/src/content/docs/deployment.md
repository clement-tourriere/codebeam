---
title: Deployment
description: Run Codebeam as a shared service today and prepare for future Docker/container deployment.
---

Codebeam currently runs as a Go binary plus static assets and templates. A supported Docker image is not part of this repository yet, but the runtime contract is already simple: one process, one persistent data directory, and environment variables.

## Production checklist

Before sharing an instance:

- Use HTTPS through a reverse proxy.
- Set `CODEBEAM_BASE_URL` to the external URL.
- Set a strong `CODEBEAM_SESSION_SECRET`.
- Set `CODEBEAM_DEV_LOGIN=false`.
- Store runtime data in a persistent, backed-up `CODEBEAM_DATA_DIR`.
- Provision the token-encryption key: set `CODEBEAM_ENCRYPTION_KEY` (or let Codebeam auto-generate a key file where the process can persist it), and back the key up **separately** from the data directory. See [Backups](#backups).
- Configure OAuth callback URLs against the external URL.
- Restrict filesystem permissions on the data directory and environment file.

## Build the application

From the repository root:

```sh
mise install
mise run build
```

This builds:

- `bin/codebeam`
- generated frontend assets in `static/`
- templates already present in `templates/`

## Install on a Linux host

Example layout:

```sh
sudo install -d -o codebeam -g codebeam /opt/codebeam
sudo install -d -o codebeam -g codebeam /var/lib/codebeam
sudo install -d -o root -g codebeam -m 0750 /etc/codebeam

sudo cp bin/codebeam /opt/codebeam/codebeam
sudo cp -R static templates /opt/codebeam/
sudo chown -R codebeam:codebeam /opt/codebeam /var/lib/codebeam
```

Create `/etc/codebeam/codebeam.env`:

```dotenv
CODEBEAM_ADDR=127.0.0.1:8080
CODEBEAM_BASE_URL=https://codebeam.example.com
CODEBEAM_DATA_DIR=/var/lib/codebeam
CODEBEAM_SESSION_SECRET=replace-with-a-long-random-value
CODEBEAM_DEV_LOGIN=false
# Encrypts stored code-host access tokens. With the hardened unit below (writable
# path limited to the data dir), set it explicitly here rather than relying on
# auto-generation. Generate with: openssl rand -base64 32
CODEBEAM_ENCRYPTION_KEY=replace-with-a-long-random-value

CODEBEAM_STATIC_DIR=/opt/codebeam/static
CODEBEAM_TEMPLATE_GLOB=/opt/codebeam/templates/*.html

CODEBEAM_GITHUB_CLIENT_ID=...
CODEBEAM_GITHUB_CLIENT_SECRET=...
```

Protect it:

```sh
sudo chown root:codebeam /etc/codebeam/codebeam.env
sudo chmod 0640 /etc/codebeam/codebeam.env
```

## systemd unit

Create `/etc/systemd/system/codebeam.service`:

```ini
[Unit]
Description=Codebeam code search
After=network-online.target
Wants=network-online.target

[Service]
User=codebeam
Group=codebeam
EnvironmentFile=/etc/codebeam/codebeam.env
WorkingDirectory=/opt/codebeam
ExecStart=/opt/codebeam/codebeam
Restart=on-failure
RestartSec=5s
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=full
ReadWritePaths=/var/lib/codebeam

[Install]
WantedBy=multi-user.target
```

Start it:

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now codebeam
sudo journalctl -u codebeam -f
```

## Reverse proxy examples

### Caddy

```text
codebeam.example.com {
  reverse_proxy 127.0.0.1:8080
}
```

### Nginx

```nginx
server {
  listen 443 ssl http2;
  server_name codebeam.example.com;

  ssl_certificate /etc/letsencrypt/live/codebeam.example.com/fullchain.pem;
  ssl_certificate_key /etc/letsencrypt/live/codebeam.example.com/privkey.pem;

  location / {
    proxy_pass http://127.0.0.1:8080;
    proxy_set_header Host $host;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto https;
  }
}
```

Set `CODEBEAM_BASE_URL=https://codebeam.example.com` and register OAuth callbacks with that same host.

## Future Docker shape

No official image is published yet. When a Dockerfile/image is added, it should follow this runtime contract:

- listen on `:8080` inside the container,
- mount a persistent volume at `/data`,
- set `CODEBEAM_DATA_DIR=/data`,
- include `static/`, `templates/`, `git`, and optionally Universal Ctags,
- pass OAuth and session settings as environment variables or secrets,
- set `CODEBEAM_ENCRYPTION_KEY` (or mount `CODEBEAM_ENCRYPTION_KEY_FILE` on the persistent volume) — a container that regenerates its key each restart cannot read previously stored tokens.

A future Compose file is expected to look like this:

```yaml
services:
  codebeam:
    image: ghcr.io/ctourriere/codebeam:latest # future image
    restart: unless-stopped
    environment:
      CODEBEAM_ADDR: ":8080"
      CODEBEAM_BASE_URL: "https://codebeam.example.com"
      CODEBEAM_DATA_DIR: "/data"
      CODEBEAM_SESSION_SECRET: "replace-with-a-long-random-value"
      CODEBEAM_ENCRYPTION_KEY: "replace-with-a-long-random-value"
      CODEBEAM_DEV_LOGIN: "false"
      CODEBEAM_GITHUB_CLIENT_ID: "..."
      CODEBEAM_GITHUB_CLIENT_SECRET: "..."
    volumes:
      - codebeam-data:/data
    ports:
      - "127.0.0.1:8080:8080"

volumes:
  codebeam-data:
```

Until then, use the binary deployment above or build your own image from the same contract.

## Backups

The safest backup is the complete `CODEBEAM_DATA_DIR` while Codebeam is stopped:

```sh
sudo systemctl stop codebeam
sudo tar -C /var/lib -czf codebeam-data-$(date +%F).tar.gz codebeam
sudo systemctl start codebeam
```

At minimum, back up `codebeam.db`; it contains the source of truth for users, tokens, repositories, and settings. Index shards can be rebuilt, but rebuilding may take time.

Code-host access tokens in the database are **encrypted at rest**, so a stolen data backup alone cannot use them — as long as the encryption key lives outside `CODEBEAM_DATA_DIR` (it does by default, and the systemd env file above keeps it in `/etc/codebeam`). The flip side: **back up the encryption key separately and securely.** If you lose it, the stored tokens are unrecoverable and every user must reconnect their code host. Never store the key inside the backed-up data directory, or a single stolen backup would contain both halves.

## Upgrades

1. Pull the new Codebeam source or release.
2. Run `mise run build`.
3. Stop Codebeam.
4. Replace the binary and updated `static/` / `templates/` assets.
5. Start Codebeam.
6. Watch logs for migration or indexing errors. On the first start after upgrading to a build with token encryption, any plaintext code-host tokens are encrypted in place automatically (a one-time, idempotent migration); reads keep working with no user action. This makes the token column one-way — an older binary without decryption support would read the encrypted values as invalid tokens.

```sh
sudo systemctl stop codebeam
sudo cp bin/codebeam /opt/codebeam/codebeam
sudo rm -rf /opt/codebeam/static /opt/codebeam/templates
sudo cp -R static templates /opt/codebeam/
sudo chown -R codebeam:codebeam /opt/codebeam
sudo systemctl start codebeam
sudo journalctl -u codebeam -f
```
