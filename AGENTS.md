# Working in Chickadee Roost

This is the public Chickadee Roost repository. Roost owns the managed-service
experience and coordination. Chickadee owns standalone host execution. Private operator repositories own deployed inventory,
credentials, and operational configuration. Keep these boundaries visible in
code, documentation, issues, and pull requests.

## Ownership

- Implement GitHub App login, onboarding, account policy, queue intent, applied
  service status, and usage presentation here.
- Keep QEMU, KVM, TAP/NAT setup, image building, guest serial control, disposable
  disks, host recovery, and local resource enforcement in Chickadee.
- Do not import Chickadee's private `internal/` packages or copy its host pool
  implementation into Roost. Host integration uses Chickadee's public workerapi v1 package. Never import
  its internal packages. Fleet execution is experimental; source and live
  acceptance are distinct.
- Preserve standalone Chickadee operation without Roost, vendor credentials,
  an external database, or a managed-service account.
- Keep operator deployment details in private operations repositories. Public
  examples must use placeholders and contain no customer/host inventory.

The standalone web-module extraction is complete. The current single-host
bridge is transitional. Its Python helpers and private
Unix admin JSON contract may remain during migration; document changes to their
compatibility rather than pretending they are a new multi-host worker protocol.

## Authentication and state

Treat GitHub responses, browser input, and imported metadata as untrusted.
Maintain bounded bodies/lists, numeric identity checks, current GitHub access
verification, exact Origin checks, CSRF, one-time OAuth state, S256 PKCE, and
Secure/HttpOnly cookies in HTTPS deployments. Do not relax these checks to hide
an integration bug. `Referrer-Policy: same-origin` preserves native form Origin
checks while preventing cross-origin referrer disclosure.

Installing the App does not establish applied runner capacity. Keep requested
and applied queues/status distinct. Portable policy defaults to manual approval;
the operator may explicitly enable automatic hosted admission with the web flag
and matching backend policy. Only verified owner/admin enrollment is eligible;
GitHub permissions and broader runner-group access remain backend decisions.
The site must never trust browser-supplied privacy, authenticated provenance, or
workflow-access mode. An `authenticated` record proves the enrollment handler
verified a user; imports force it false, and backend current-role checks remain
necessary before automatic admission. Supported queues stay opt-in and disabled
until the controller acknowledges their applied configuration. Keep runner
labels stable and avoid physical-host selectors until a product requirement
justifies one.

Do not log or commit App keys, OAuth client secrets, user/installation tokens,
JIT configuration, raw upstream errors, customer records, generated images, or
runtime files. The website does not need the host's App private key. Preserve
current token-memory/session-expiry behavior unless a separately reviewed design
changes credential storage.

Usage graphs must use real bounded data. Reserved VM time is not GitHub job
execution time or a billing ledger. Never sum repeated organization snapshots;
future multi-host aggregation needs worker identities and deduplication before
publishing a scope total.

## Migration compatibility

Until a deliberate operator migration, retain the `chickadee-web` executable,
`CHICKADEE_*` environment variables, service account/unit and private state/socket
paths. Do not rename cookies, callback URLs, JSON fields, or stored records as a
side effect of extracting the module. An intentional change needs compatibility
handling and deployment instructions.

The website remains standard-library Go. The fleet broker uses the pinned
GitHub scaleset client and Chickadee public worker API; keep those versions
reproducible and do not add host-engine dependencies. Keep the small HTML/CSS/JavaScript interface
accessible on mobile and preserve truthful empty/pending/error states.

## Verification

Run `make build test`: Go race tests and Python standard-library unit tests.
Use mock GitHub responses for authentication/admission tests; do not change real
App installations, runner groups, or customer workflows as part of ordinary
verification. Confirm private admin routes remain absent from public HTTP.
Use browser checks for native forms, copy controls, layout, and redirects when
those behaviors change. Never substitute manually constructed Origin headers
for a browser regression check of native form behavior.

Source or recipe completion is not deployment evidence. State which tests passed,
which live behavior was verified, and what remains pending. Publishing a module
or adding a proposal does not implement fleet scheduling. Do not deploy or mutate
host networking merely because a code change would benefit from a live test;
follow the user's existing authorization and the operator's deployment process.
