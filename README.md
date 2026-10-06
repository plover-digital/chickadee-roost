# Chickadee Roost

Roost is the service layer behind `chickadee.run`: GitHub App sign-in,
repository/organization onboarding, approved runner-queue selection, applied
status, and basic usage graphs. Runner execution remains in Chickadee.

The web module and operator bridge have been extracted into this public
repository from the working one-host beta. This boundary change does not implement
a multi-host scheduler, worker RPC, billing, or a production
hostile-tenant isolation boundary. See the
[host fleet proposal](https://github.com/plover-digital/chickadee/blob/main/docs/multi-host.md).

## Repository boundaries

| Repository | Responsibility | Visibility |
| --- | --- | --- |
| [Chickadee](https://github.com/plover-digital/chickadee) | Standalone Linux host controller, QEMU/KVM lifecycle, guest bootstrap, image builders, local recovery and resource limits | Public |
| [Chickadee Roost](https://github.com/plover-digital/chickadee-roost) | GitHub authentication, customer onboarding, service policy, desired/applied queue status and usage presentation | Public |
| Operator deployment repository | Host/cloud inventory, deployment automation, configuration, rollout and incident notes | Private |

Someone can clone Chickadee and operate their own runners without Roost or a
hosted-service account. Roost runs separately and does not manage QEMU processes,
TAP devices, or VM disks inside its web service. Public source describes the
portable software; deployment credentials and customer records stay outside Git.

## Current integration

The web server uses Go's standard library and embeds its HTML/CSS/JavaScript.
GitHub App OAuth uses browser-bound state and S256 PKCE. The OAuth client secret
stays on the web host; user access tokens remain in expiring memory sessions.
The website does not need the runner controller's App private key.

A private Unix-socket admin interface receives approved metadata and applied
status. The existing optional Python reconciler verifies GitHub access and
updates one Chickadee host through the documented operator bridge. This is a
migration-compatible integration, not a distributed worker API. Repository and
organization requests retain stable `chickadee` runner labels; customers do not
select a physical host.

New accounts request beta approval and start with only `chickadee`. Approved
accounts can opt into supported queues. Requested selection remains distinct
from applied availability. Organization usage describes its shared scope;
personal repositories remain separate. The graph shows completed reserved VM
time, including runner connection and cleanup, not billable job minutes.

## Build and test

Use Go 1.26.3 for the reference build and Python 3 for the operator scripts.
The Go web package has no external module dependencies. Python scripts use the
standard library; operational App signing also requires OpenSSL.

```sh
make build test
```

Equivalent checks are `go test -race ./...` and
`python3 -m unittest discover -s scripts -p 'test_*.py'`. Tests use synthetic
GitHub responses and private temporary files, not live customer accounts.

For deployment compatibility, the executable remains `chickadee-web`. The existing
`CHICKADEE_*` environment variables, `chickadee-web` service account/unit,
`/var/lib/chickadee-web` state directory, and `admin.sock` location are retained
so operators can migrate the binary without silently changing their deployment.
The GitHub callback remains `/auth/github/callback` and setup route `/setup`.
See [onboarding configuration](docs/onboarding-site.md) and
[authentication](docs/authentication.md).

Production deployment requires HTTPS, a private regular OAuth-secret file,
private state storage, and an operator-reviewed admission policy. Never expose
the Unix admin interface through the public reverse proxy. Keep deployment
inventory, App keys, OAuth secrets, customer JSON, and runtime artifacts out of
this repository.

## Development

See [AGENTS.md](AGENTS.md) for the repository boundaries and contribution checks.
Future fleet integration should use an explicit versioned API with worker
identity and acknowledgement; it must preserve independent Chickadee operation.
The current trusted beta does not imply an unattended public compute service or
an isolation/reliability guarantee.
