---
title: Deployment
description: Run Codebeam as a shared service for your team, with Docker or a plain binary behind a reverse proxy.
---

A shared Codebeam instance is deliberately boring to run: **one process, one data directory, no external services**. Deploy the Docker image or the self-contained binary, put a reverse proxy in front for HTTPS, and back up one directory. `git` must be available at runtime — Codebeam shells out to it to clone and read repositories (the Docker image includes it, plus Universal Ctags for symbol search).

## Production checklist

Before pointing teammates at an instance:

- Serve **HTTPS** through a reverse proxy, and set `CODEBEAM_BASE_URL` to the external URL.
- Set a strong `CODEBEAM_SESSION_SECRET`.
- Set `CODEBEAM_DEV_LOGIN=false` explicitly.
- Put the data in a persistent, backed-up `CODEBEAM_DATA_DIR`.
- Set `CODEBEAM_ENCRYPTION_KEY` explicitly (or persist the auto-generated key file) and back the key up **separately** from the data — see [Backups](#backups).
- Configure [OAuth or SSO](/codebeam/oauth/) with callbacks against the external URL.
- Restrict filesystem permissions on the data directory and environment file.

## Docker

Multi-arch images (linux/amd64, linux/arm64) are published to GHCR on every release: `ghcr.io/clement-tourriere/codebeam` with `latest`, `X.Y`, and `X.Y.Z` tags. The image listens on `:8080` and uses **two volumes**:

- `/data` — database, cloned repositories, and indexes (`CODEBEAM_DATA_DIR`).
- `/config` — the auto-generated token-encryption key, deliberately separate from `/data` so a stolen data backup does not also carry the key. Persist both; losing `/config` makes stored code-host tokens unreadable (or set `CODEBEAM_ENCRYPTION_KEY` explicitly instead).

```yaml
services:
  codebeam:
    image: ghcr.io/clement-tourriere/codebeam:latest
    restart: unless-stopped
    environment:
      CODEBEAM_BASE_URL: "https://codebeam.example.com"
      CODEBEAM_SESSION_SECRET: "replace-with-a-long-random-value"
      CODEBEAM_DEV_LOGIN: "false"
      CODEBEAM_GITHUB_CLIENT_ID: "..."
      CODEBEAM_GITHUB_CLIENT_SECRET: "..."
    volumes:
      - codebeam-data:/data
      - codebeam-config:/config
    ports:
      - "127.0.0.1:8080:8080"

volumes:
  codebeam-data:
  codebeam-config:
```

To index repositories already on the host, bind-mount them (read-only is fine) and add them as local repositories from the UI.

## Binary on a Linux host

Released binaries are self-contained — web UI embedded, a single file is enough:

```sh
curl -fsSL https://raw.githubusercontent.com/clement-tourriere/codebeam/main/install.sh | sh
```

(or download from the [releases page](https://github.com/clement-tourriere/codebeam/releases), or build with `mise run build`). Make sure `git` — and optionally `universal-ctags` — is installed, then set up directories for a dedicated user:

```sh
sudo install -d -o codebeam -g codebeam /opt/codebeam
sudo install -d -o codebeam -g codebeam /var/lib/codebeam
sudo install -d -o root -g codebeam -m 0750 /etc/codebeam

sudo install -m 0755 codebeam /opt/codebeam/codebeam
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

CODEBEAM_GITHUB_CLIENT_ID=...
CODEBEAM_GITHUB_CLIENT_SECRET=...
```

Protect it:

```sh
sudo chown root:codebeam /etc/codebeam/codebeam.env
sudo chmod 0640 /etc/codebeam/codebeam.env
```

### systemd unit

`/etc/systemd/system/codebeam.service`:

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

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now codebeam
sudo journalctl -u codebeam -f
```

## Reverse proxy

**Caddy** (automatic HTTPS):

```text
codebeam.example.com {
  reverse_proxy 127.0.0.1:8080
}
```

**Nginx**:

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

## Backups

The safest backup is the complete data directory while Codebeam is stopped:

```sh
sudo systemctl stop codebeam
sudo tar -C /var/lib -czf codebeam-data-$(date +%F).tar.gz codebeam
sudo systemctl start codebeam
```

At minimum, back up `codebeam.db` — it is the source of truth for users, tokens, repositories, and settings. Clones and index shards can be rebuilt, though rebuilding takes time.

Code-host tokens in the database are **encrypted at rest**, so a stolen data backup alone cannot use them — as long as the encryption key lives outside `CODEBEAM_DATA_DIR` (it does by default; the systemd setup above keeps it in `/etc/codebeam`). The flip side: **back up the key separately and securely.** Lose it and the stored tokens are unrecoverable — every user must reconnect their code host. Never store the key inside the backed-up data directory, or one stolen backup carries both halves.

## Upgrades

With Docker, pull the new tag and recreate the container. With the binary:

```sh
sudo systemctl stop codebeam
sudo install -m 0755 codebeam /opt/codebeam/codebeam
sudo systemctl start codebeam
sudo journalctl -u codebeam -f
```

Web assets are embedded, so replacing the binary is the whole upgrade. Watch the logs for migration errors on first start. When upgrading from a build without token encryption, any plaintext code-host tokens are encrypted in place automatically (a one-time, idempotent migration) — note this makes the token column one-way; an older binary can no longer read those values.

Check the running version any time with `codebeam version`.
