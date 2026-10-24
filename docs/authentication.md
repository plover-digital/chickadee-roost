# GitHub authentication

Chickadee uses the GitHub App web authorization flow for **Continue with GitHub**,
then a separate GitHub App installation to grant access to selected repositories.
Users do not create a Chickadee password or provide a personal access token.

[Blacksmith describes the same GitHub-only sign-in model](https://www.blacksmith.sh/security).
Its publicly documented integration is organization-focused. Chickadee also supports
personal repositories; their runner-management API requires repository Administration
write permission. Organization runner management requires Self-hosted runners write.
We do not request email access or migration permissions for editing repository code.

## Identity and access

The site sends the browser to GitHub's `/login/oauth/authorize` endpoint with a random,
one-time state, a browser-bound nonce cookie, the configured callback URL, and S256
PKCE. The callback exchanges the code server-side using the App's OAuth client secret
and the PKCE verifier, then verifies the identity through `GET /user`.

The resulting user access token stays in memory in a one-hour session. Tokens are not
stored in enrollment JSON, browser storage, or guest VMs. HTTPS cookies use the
`__Host-` prefix, Secure, HttpOnly, and SameSite=Lax. Restarting the web service requires
users to sign in again. Refresh tokens are not retained unless persistent agent
delegation is explicitly enabled as described below.

The dashboard reads installations and repositories through GitHub using that user's
App token. Repository administration permission is required to select a repository;
a personal installation also requires the signed-in user to own the installation.
Repository access alone does not authorize service management. Enrollment and management
requests recheck GitHub access and require both a CSRF token and the exact site Origin.
GitHub setup callback parameters are not evidence of identity or installation ownership.

The website does not receive the runner controller's App private key. Installing or
authorizing the App does not establish runner availability. Operators can choose
manual approval (the portable default) or explicitly enable automatic hosted
activation for verified account owners/repository administrators. The web flag
and backend policy must agree; neither path changes GitHub permissions. Host provisioning uses installation credentials,
and disposable guests receive only per-runner JIT configuration.

Authenticated enrollment records contain a server-assigned `authenticated: true`.
The browser cannot grant that provenance: it is set only after session, current
repository-admin/owner, App permission, CSRF, and Origin checks. Operator imports
force the field false. This records enrollment-time verification, not perpetual
GitHub access; automatic backend admission must recheck current access. User
tokens are not included in the operator status API.

Native HTML forms require the `same-origin` referrer policy so browsers send a
usable same-origin Origin header. `Origin: null`, foreign origins, and invalid
CSRF still fail closed. OAuth callback state, browser nonce, and PKCE checks are
unchanged by automatic activation.

## Agent delegation

Persistent agent API access is explicitly enabled with a private 32-byte
`CHICKADEE_AGENT_KEY_FILE`. Signed-in users create named tokens for selected
repositories and read/manage scope at `/settings/tokens`, with expiry from 1 to
90 days. This is a deliberate extension of the memory-only browser credential
model: token digests and delegated GitHub access/refresh grants are stored in a
versioned authenticated-encryption file in private web state. The encryption key
stays outside source and state backups. Raw Chickadee token values are shown once
and never stored. Token pages omit third-party scripts.

Agent tokens survive browser logout and website restarts; the user can revoke
them from any current session. They can narrow but never expand current GitHub
access. API repository reads and mutations intersect selected repository IDs
with live GitHub administration checks. Refresh exchanges verify numeric identity
and save rotated credentials before access continues. GitHub authorization
revocation or expiry can require human sign-in again. Normal browser sessions
retain their one-hour lifetime; without agent delegation enabled, they retain
no refresh tokens and no persistent GitHub credentials.

Creation and revocation require browser CSRF and exact Origin checks. `/api/v1/`
requires explicit Bearer authentication, never cookies or query tokens. Service
mutations share existing browser authorization and admission logic. See the
[credential design and rollout](agent-api.md), which must be reviewed before
operator enablement. The website still never receives the runner App private key.

## App settings for a deployment

Use your own public GitHub App and configure:

- Callback URL: `https://YOUR-DOMAIN/auth/github/callback`.
- Setup URL: `https://YOUR-DOMAIN/setup`.
- Repository Administration read/write for personal repository runners.
- Organization Self-hosted runners read/write for organization runners.
- Leave callback wildcard matching and Device Flow disabled.
- Keep user access token expiration enabled.

Generate an **OAuth client secret** in the GitHub App settings and store it in a file
readable only by the web-service user. Set `CHICKADEE_OAUTH_SECRET_FILE` to that file.
This secret is different from the App's private PEM key and is required even when
using PKCE. Neither belongs in Git, workflow YAML, support tickets, or chat.
See [onboarding-site.md](onboarding-site.md) for the web-service configuration.

## Validation

Tests cover PKCE and callback binding, expired and cross-browser state rejection,
one-time callback replay rejection, safe cookies, upstream error handling without
credential reflection, forged repository enrollment rejection, and GitHub-admin access
checks. A live deployment still needs an owner-authorized browser sign-in to verify
its actual GitHub callback registration and client secret. REST App metadata does not
expose those callback settings.

Official references: [GitHub App user access tokens](https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/generating-a-user-access-token-for-a-github-app),
[installation and accessible-repository APIs](https://docs.github.com/en/rest/apps/installations).
