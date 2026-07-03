---
title: Deployment
description: Run Codebeam as a shared service with Docker or a self-contained binary.
---

Codebeam runs as a single process with a persistent data directory, configured through environment variables. Two supported deployment shapes: the official Docker image, or a self-contained release binary. Either way, `git` must be available at runtime — Codebeam shells out to it to clone, fetch, and read repositories (the Docker image includes it, plus Universal Ctags for symbol indexing).

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

## Get the binary

Released binaries are self-contained — the web UI assets are embedded, so a single file is enough:

```sh
curl -fsSL https://raw.githubusercontent.com/clement-tourriere/codebeam/main/install.sh | sh
```

Or download an archive from the [releases page](https://github.com/clement-tourriere/codebeam/releases). To build from source instead, run `mise install && mise run build` from the repository root (produces `bin/codebeam`).

## Install on a Linux host

Make sure `git` (and optionally `universal-ctags`) is installed, then:

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

## Docker

Multi-arch images (linux/amd64, linux/arm64) are published to GHCR on every release: `ghcr.io/clement-tourriere/codebeam` with `latest`, `X.Y`, and `X.Y.Z` tags. The image includes `git` and Universal Ctags, listens on `:8080`, and uses **two volumes**:

- `/data` — SQLite database, cloned repos, and search indexes (`CODEBEAM_DATA_DIR`).
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

To index repositories already on the host, bind-mount them (read-only is fine for indexing) and add them as local repositories from the UI.

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

With Docker, pull the new image tag and recreate the container. With the binary:

1. Download the new release (or rerun the install script).
2. Stop Codebeam, replace the binary, start Codebeam — the web assets are embedded, there is nothing else to copy.
3. Watch logs for migration or indexing errors. On the first start after upgrading to a build with token encryption, any plaintext code-host tokens are encrypted in place automatically (a one-time, idempotent migration); reads keep working with no user action. This makes the token column one-way — an older binary without decryption support would read the encrypted values as invalid tokens.

```sh
sudo systemctl stop codebeam
sudo install -m 0755 codebeam /opt/codebeam/codebeam
sudo systemctl start codebeam
sudo journalctl -u codebeam -f
```
