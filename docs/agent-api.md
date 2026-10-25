# Agent access

GitHub remains the only human sign-in method. After sign-in and GitHub App
installation, open `/settings/tokens` to create a named token. Select its
repositories, read-only or management permission, and expiry: 1, 7, 30 or 90 days,
with 30 days selected by default. The token is shown once. Put it in the agent's
secret store as `CHICKADEE_TOKEN`, then point the agent to `/llms.txt` on this
site. `/agents` provides the human introduction.

The complete contract and examples live in the embedded [agent guide](../internal/site/assets/agents.md),
served at `/agents.md`. The [OpenAPI description](../internal/site/assets/openapi.json)
is served publicly at `/openapi.json`; `/llms.txt` links both. No MCP server or
OAuth device-login CLI is included in this version. Terminal agents can use
ordinary HTTPS clients. GitHub still receives workflows and dispatches jobs.

## Delegated credential design

Agent tokens deliberately outlive the existing one-hour browser session and web
restarts. Browser logout does not revoke them; use the account-wide token list
to revoke access. Each token has a maximum lifetime of 90 days and is restricted
to selected repository IDs. Discovery and mutation recheck current GitHub admin
access and intersect it with those IDs. New repositories never expand an existing
token. Current organization restrictions and admission policy still apply.
Revocation prevents subsequent requests; an already authorized in-flight request
may finish. The API cannot mint or refresh Chickadee tokens.

Raw Chickadee tokens are never stored. SHA-256 token digests, names, repository
restrictions, expiry and the delegated GitHub grants are stored in
`agent-credentials.enc` in the private web state directory. The whole version-1
store is authenticated and encrypted using AES-256-GCM and a fresh random nonce
on each atomic, fsynced replacement. The service refuses corrupt ciphertext,
wrong keys, unsupported versions and unsafe file permissions at startup.

Persistent agent access is an explicit opt-in. Configure
`CHICKADEE_AGENT_KEY_FILE` to a private regular file containing exactly 32 random
**binary** bytes. Keep this key outside the source checkout and separate from
state-file backups. Limit it to the web service user; never commit, log or paste
it into a ticket. For example, as part of the operator's credential setup:

```sh
umask 077
openssl rand -out /PRIVATE-CREDENTIAL-DIRECTORY/chickadee-agent.key 32
```

Set the environment variable to that absolute path through the existing service
configuration. Do not regenerate or overwrite an existing key during upgrades.
Losing the key loses access to the encrypted grants; replacing it without an
explicit reset causes startup to fail closed. Key rotation is not automated in
v1. An operator reset requires stopping the website, removing its encrypted agent
store through the private operations process, and issuing new tokens afterward.
Normal GitHub browser sign-in works without enabling agent credentials.

When enabled, OAuth callbacks retain the GitHub refresh grant in the expiring
memory session, and persist it only when the human explicitly creates an agent
token. Creating tokens from the same sign-in shares one underlying grant.
The server refreshes expiring GitHub access tokens, verifies numeric user identity
following the exchange, and persists the rotated credentials with identity
verification pending before querying GitHub. Transient identity failures return a
retryable error; verification remains required after a restart. Only a confirmed
matching identity clears that gate. A failed save does not replay the consumed
refresh token. The
credential-store mutex is separate from the website session mutex. Revoking the
last token for a grant removes its stored GitHub credentials. Expired metadata
is pruned during access and store updates; the next successful update persists
that pruning. Neither GitHub nor Chickadee secret values enter API responses,
enrollment metadata, analytics or guest VMs. Token pages exclude third-party
scripts entirely.

GitHub can revoke the grant independently; refresh denial or identity mismatch
fails closed and requires new human authorization. A transient refresh outage
returns a retryable error without reflecting GitHub's body. This design depends
on the existing configured GitHub OAuth client secret and expiring App user
tokens; it does not require the App private key or a new GitHub permission.
Existing sign-ins without refresh metadata must sign in again to create tokens.

## Compatibility and rollout

This adds the Roost service API **v1** under `/api/v1/` and encrypted agent-state
format **1**. Workerapi v1, stored enrollment fields, cookies, callback URL,
operator bridge, broker, standalone operation and service admission stay
compatible. No external database is added. No engine repository change is needed.

1. Review the credential design and source, then provision the encryption key
   through private operator tooling. Preserve existing state and credentials.
2. Deploy the web binary and embedded assets together with the new optional
   environment variable. A normal web restart requires browser sign-in again;
   once provisioned, agent tokens survive subsequent restarts.
3. Check `/agents`, `/llms.txt`, `/agents.md` and `/openapi.json`. Sign in, create a
   read-only token for a selected test repository, confirm authorized API reads
   and out-of-scope denial, then revoke and verify denial.
4. Record the deployed source version and actual live verification separately.
   No controller, broker or worker deployment is required.

A proxy path allowlist must permit `/agents`, `/agents.md`, `/llms.txt`,
`/openapi.json`, `/settings/tokens`, `/settings/tokens/revoke` and `/api/v1/`.
Keep private Unix admin/status/import/telemetry routes absent from public HTTP.
Rollback to an older web binary leaves the encrypted store unused; it does not
revoke saved tokens. Explicitly revoke/reset grants before retiring the feature.
