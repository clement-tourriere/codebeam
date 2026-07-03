---
title: Authentication and access
description: Sign-in methods, code-host connections, SSO, API tokens, roles, and permission sync.
---

Codebeam has two related but distinct kinds of authentication:

1. **Signing in to Codebeam** — dev login, GitHub/GitLab OAuth, or OIDC single sign-on.
2. **Connecting a code host** — giving Codebeam access to list and clone your repositories, via OAuth or a personal access token.

GitHub/GitLab OAuth conveniently does both at once. This page covers all the options, plus what happens after sign-in: roles, repository permissions, and Codebeam's own API tokens.

## Choosing a method

| Method | Signs you in | Fetches private repos | Setup |
| --- | --- | --- | --- |
| Development login | ✅ | — | None (localhost only by default) |
| GitHub / GitLab OAuth | ✅ | ✅ | Create an OAuth app once |
| OIDC SSO (Okta, Entra ID, …) | ✅ | — | Create an OIDC app once |
| GitHub PAT | — | ✅ | Paste a token on `/sources` |
| Self-managed GitLab PAT | — | ✅ | Paste a token on `/sources` |

For a laptop, development login plus a PAT is zero-configuration. For a team server, configure OAuth and/or SSO and let each user connect their own code-host account.

## Development login

The **Continue in development mode** button signs you in with no password. It is enabled automatically only when `CODEBEAM_BASE_URL` is a loopback address (localhost), and disabled for any real host — so a reachable instance never ships an unauthenticated door. Override explicitly with `CODEBEAM_DEV_LOGIN=true|false`.

The first user to sign in — by any method — becomes the instance **admin**.

## OAuth callback URLs

All OAuth flows redirect back to Codebeam at:

```text
<CODEBEAM_BASE_URL>/auth/<provider>/callback
```

e.g. `https://codebeam.example.com/auth/github/callback`. The single most common OAuth failure is a mismatch here: `CODEBEAM_BASE_URL` must exactly match the URL users open in the browser (scheme, host, and port).

## GitHub OAuth

1. On GitHub: **Settings → Developer settings → OAuth Apps → New OAuth App**.
2. Set the **Homepage URL** to your `CODEBEAM_BASE_URL` and the **Authorization callback URL** to `<CODEBEAM_BASE_URL>/auth/github/callback`.
3. Copy the client ID, generate a client secret, and add both to the environment:

```dotenv
CODEBEAM_GITHUB_CLIENT_ID=...
CODEBEAM_GITHUB_CLIENT_SECRET=...
```

Restart Codebeam; GitHub appears on the login page and on `/sources`. Codebeam requests the `read:user`, `user:email`, and `repo` scopes — `repo` is what lets it list and clone the private repositories the user can access.

## GitLab OAuth

1. On GitLab (**Preferences → Applications**, or an instance/group-level application), create an app with redirect URI `<CODEBEAM_BASE_URL>/auth/gitlab/callback` and scopes `read_user`, `read_api`, `read_repository`. Keep it confidential so GitLab issues a secret.
2. Configure Codebeam:

```dotenv
CODEBEAM_GITLAB_BASE_URL=https://gitlab.com   # or https://gitlab.company.com
CODEBEAM_GITLAB_CLIENT_ID=...
CODEBEAM_GITLAB_CLIENT_SECRET=...
```

`CODEBEAM_GITLAB_BASE_URL` selects GitLab.com or one self-managed host — OAuth supports a single GitLab host per instance. Need several self-managed GitLabs? Use the [token flow](#self-managed-gitlab-pat) instead; each instance connects independently.

## OIDC single sign-on

For company deployments, Codebeam signs users in against any spec-compliant OpenID Connect provider — Okta, Microsoft Entra ID, Google Workspace, Keycloak, Authelia — using the authorization-code flow with PKCE.

1. Create a **web** application in your provider with redirect URI `<CODEBEAM_BASE_URL>/auth/oidc/callback`.
2. Configure:

```dotenv
CODEBEAM_OIDC_ISSUER=https://your-tenant.okta.com
CODEBEAM_OIDC_CLIENT_ID=...
CODEBEAM_OIDC_CLIENT_SECRET=...
CODEBEAM_OIDC_NAME=Okta                      # login button label
CODEBEAM_OIDC_ALLOWED_DOMAINS=acme.com       # optional email-domain allowlist
```

Endpoints are discovered from `{issuer}/.well-known/openid-configuration` — nothing else to configure. The login page shows **Continue with Okta**, and users are provisioned on first sign-in. SSO handles *identity only*; users still connect a code host (OAuth or PAT) to index private repositories.

For a locked-down deployment, combine SSO with `CODEBEAM_DEV_LOGIN=false` so SSO and/or code-host OAuth are the only ways in.

## Personal access tokens for code hosts

PAT connections are per-user, entered on `/sources`, and need no app-wide configuration.

### GitHub PAT

Any token that can read the repositories you want to index:

- **Classic**: `repo` (plus `read:user`) covers private repository sync.
- **Fine-grained**: grant *metadata* and *contents* read access to the repositories you want.

After saving, click **Sync repositories** and pick repositories on `/repos/manage`.

### Self-managed GitLab PAT

Enter the instance URL (e.g. `https://gitlab.company.com`) and a token with `read_api` and `read_repository`. Each self-managed instance is tracked as its own source, so you can connect several.

## Roles and permissions

**Two roles.** The first user becomes the **admin**; everyone after joins as a **member**. Admins manage instance settings (the Indexing tab), register local-disk repositories, and change user roles on **Settings → Users** (the last admin can't be demoted). Everything else — searching, connecting code hosts, API tokens — is available to every user.

**Repository access follows the code host.** Each user sees only the repositories their connected accounts can access. A background sync (hourly by default) mirrors changes: gain access to a private repository on GitHub and it appears in your Codebeam search; lose it and it disappears — no manual re-sync. Public repositories are visible to everyone and never pruned. Tune with `CODEBEAM_SYNC_PERMISSIONS` and `CODEBEAM_PERMISSION_SYNC_INTERVAL`.

**Tokens are encrypted at rest.** Code-host tokens are stored AES-encrypted, with the key kept *outside* the data directory — see [Configuration](/codebeam/configuration/#server-and-authentication).

## Codebeam API tokens

For scripts and agents calling the [JSON API or the HTTP MCP endpoint](/codebeam/integrations/), create a personal access token under **Settings → API tokens**. Tokens are prefixed `cbp_`, shown once, stored only as hashes, can expire, and can be revoked. A token carries *your* repository permissions — it sees exactly what you see.

MCP clients using OAuth instead of a token appear under **Settings → Connected agents**, where each grant can be revoked.

## Troubleshooting sign-in

| Symptom | Check |
| --- | --- |
| Provider button missing or disabled | Both client ID and secret must be set, and Codebeam restarted. |
| Redirect URI mismatch | The provider's callback must exactly equal `<CODEBEAM_BASE_URL>/auth/<provider>/callback`. |
| OAuth works locally but fails behind a proxy | Set `CODEBEAM_BASE_URL` to the external HTTPS URL, not the listen address. |
| GitLab OAuth goes to the wrong host | Set `CODEBEAM_GITLAB_BASE_URL` and restart. |
| Private repositories missing after sync | Verify token/OAuth scopes and that the account actually has access. |
| Callback reports invalid state | Allow cookies, and make sure the same instance handles the start and the callback. |
