---
title: OAuth and tokens
description: Configure GitHub, GitLab.com, self-managed GitLab, and personal access token connections.
---

Codebeam supports several ways to authenticate users and fetch private repositories:

| Method | Best for | Requires app-wide env vars? |
| --- | --- | --- |
| Development login | Local testing and single-user experiments. | No |
| Public GitHub URL | Indexing public GitHub repositories. | No |
| Personal access token (PAT) | Connecting one user's GitHub or self-managed GitLab repositories without setting up OAuth. | No |
| GitHub OAuth | Shared login and repository sync for GitHub. | Yes |
| GitLab OAuth | Shared login and repository sync for GitLab.com or one configured self-managed GitLab host. | Yes |

:::caution[Disable development login on shared instances]
Set `CODEBEAM_DEV_LOGIN=false` before exposing Codebeam to a team or the internet.
:::

## Callback URL format

Codebeam builds OAuth callback URLs from `CODEBEAM_BASE_URL`:

```text
<CODEBEAM_BASE_URL>/auth/<provider>/callback
```

Examples:

```text
http://localhost:8080/auth/github/callback
http://localhost:8080/auth/gitlab/callback
https://codebeam.example.com/auth/github/callback
https://codebeam.example.com/auth/gitlab/callback
```

If OAuth fails with a redirect or callback mismatch, first check that `CODEBEAM_BASE_URL` exactly matches the URL users open in the browser.

## GitHub OAuth

Create a GitHub OAuth App:

1. Open GitHub.
2. Go to **Settings** → **Developer settings** → **OAuth Apps** → **New OAuth App**.
3. Fill in:
   - **Application name:** `Codebeam`
   - **Homepage URL:** your `CODEBEAM_BASE_URL`, for example `http://localhost:8080` or `https://codebeam.example.com`
   - **Authorization callback URL:** `<CODEBEAM_BASE_URL>/auth/github/callback`
4. Create the app.
5. Copy the client ID and generate a client secret.

Add them to `.env`:

```dotenv
CODEBEAM_BASE_URL=http://localhost:8080
CODEBEAM_GITHUB_CLIENT_ID=...
CODEBEAM_GITHUB_CLIENT_SECRET=...
```

Restart Codebeam. The login page and `/sources` page will show GitHub OAuth as available.

### GitHub scopes

Codebeam requests these GitHub OAuth scopes:

- `read:user`
- `user:email`
- `repo`

`repo` is required so Codebeam can list and clone private repositories the user can access.

## GitLab.com OAuth

Create a GitLab OAuth application:

1. Open GitLab.com.
2. Go to **Preferences** → **Applications**.
3. Create a new application:
   - **Name:** `Codebeam`
   - **Redirect URI:** `<CODEBEAM_BASE_URL>/auth/gitlab/callback`
   - **Scopes:** `read_user`, `read_api`, `read_repository`
   - Keep the app confidential so GitLab issues a secret.
4. Copy the application ID and secret.

Add them to `.env`:

```dotenv
CODEBEAM_BASE_URL=http://localhost:8080
CODEBEAM_GITLAB_BASE_URL=https://gitlab.com
CODEBEAM_GITLAB_CLIENT_ID=...
CODEBEAM_GITLAB_CLIENT_SECRET=...
```

Restart Codebeam and connect from `/login` or `/sources`.

## Self-managed GitLab OAuth

For OAuth against a self-managed GitLab instance, set `CODEBEAM_GITLAB_BASE_URL` before starting Codebeam:

```dotenv
CODEBEAM_BASE_URL=https://codebeam.example.com
CODEBEAM_GITLAB_BASE_URL=https://gitlab.company.com
CODEBEAM_GITLAB_CLIENT_ID=...
CODEBEAM_GITLAB_CLIENT_SECRET=...
```

Create the OAuth application on `https://gitlab.company.com` with this redirect URI:

```text
https://codebeam.example.com/auth/gitlab/callback
```

Codebeam currently supports one app-wide GitLab OAuth host at a time. If you need to connect several self-managed GitLab instances, use the self-managed GitLab token flow in `/sources`.

## OIDC single sign-on

For company deployments, Codebeam signs users in against any spec-compliant OpenID Connect provider — Okta, Microsoft Entra ID, Google Workspace, Keycloak, Authelia, and friends — using the authorization-code flow with PKCE, `state`, and `nonce`.

1. Create a **web** application in your provider with the redirect URI `https://your-codebeam-host/auth/oidc/callback`.
2. Configure Codebeam:

```dotenv
CODEBEAM_OIDC_ISSUER=https://your-tenant.okta.com
CODEBEAM_OIDC_CLIENT_ID=...
CODEBEAM_OIDC_CLIENT_SECRET=...
CODEBEAM_OIDC_NAME=Okta
# Optionally restrict sign-in to company email domains:
CODEBEAM_OIDC_ALLOWED_DOMAINS=acme.com
```

The login page then shows **Continue with Okta**. Users are provisioned on first sign-in: the very first user of the instance becomes the admin, everyone after joins as a member. Endpoints are discovered from `{issuer}/.well-known/openid-configuration`, so there is nothing else to configure.

For a locked-down deployment, combine SSO with `CODEBEAM_DEV_LOGIN=false` so the SSO (and/or code-host OAuth) buttons are the only way in.

## Personal access tokens

PAT connections are per-user and are entered from `/sources`. They do not require app-wide OAuth variables.

### GitHub PAT

Use a token that can read the repositories you want to index.

- Classic token: `repo` and `read:user` are sufficient for private repository sync.
- Fine-grained token: grant repository metadata and contents read access for the repositories you want Codebeam to index.

After saving the token, click **Sync repositories**, then choose repositories from `/repos/manage`.

### Self-managed GitLab PAT

Use a token with:

- `read_api`
- `read_repository`

In `/sources`, enter the GitLab instance URL, for example `https://gitlab.company.com`, and the token. Codebeam will connect, sync repositories, and treat that instance separately from GitLab.com.

## Public GitHub repositories without auth

You can add public GitHub repositories from `/sources` without OAuth or a token. Paste either:

```text
sourcegraph/zoekt
https://github.com/sourcegraph/zoekt
```

Optionally enter comma-separated branches, such as:

```text
main,release/1.x
```

Leave the branch field empty to index the repository's default branch.

## OAuth troubleshooting

| Symptom | Check |
| --- | --- |
| Provider button says OAuth is not configured | Both client ID and secret must be non-empty in the environment, and Codebeam must be restarted. |
| Redirect URI mismatch | The provider's callback URL must exactly match `CODEBEAM_BASE_URL + /auth/<provider>/callback`. |
| OAuth works locally but fails behind a proxy | Set `CODEBEAM_BASE_URL` to the external HTTPS URL, not the internal listen address. |
| GitLab OAuth sends users to the wrong host | Set `CODEBEAM_GITLAB_BASE_URL` to the intended GitLab host and restart. |
| Private repositories do not appear | Verify OAuth/PAT scopes and that the signed-in user has access to the repositories. |
| Callback reports invalid OAuth state | Make sure browser cookies are allowed and the same Codebeam instance handles the start and callback requests. |
