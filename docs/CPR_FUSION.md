# CPR Fusion

Version: `2.17.6-cpr.1`, a source preview. No existing service is replaced or
automatically migrated. This integration does not require a modified CPR image.

## Provenance

Base: ranxi2001/sub2api v2.17.6, commit
`a7ba38d873ff97faf3a127a97ab808d8860bb8b8`.

CPR account-management components are adapted from KlN-4096/sub2api
v0.2.19-klno.1, commit `bdff1129ffbd22947612f83cd09d23bf3fa47f5d`,
under the included LGPL v3 license. Original copyright and
license notices are retained. This is a separately maintained derivative,
not an official release of either upstream.

HTTP/SSE forwarding is based on the previously developed rawproxy transport.
The target integration retains Ranxi's TTFT definition, financial journal,
authorization, tenant boundaries and lifecycle hooks. CPR owns upstream
identity and request transformations. No turn-state generation, storage,
replay or substitution is introduced.

## Account Setup

1. Build this checkout, rather than installing a binary from an upstream README
   link. The frontend build requires Node 22 or newer and pnpm 9; the Go toolchain is pinned by
   `backend/go.mod`. The existing Dockerfile can build the complete application.
2. Open Accounts and choose **+ CPR**. Enter a name, CPR inference URL and
   CPR Admin Key. An optional separate admin URL supports split deployments.
   If omitted, a trailing `/v1` is removed from the inference URL for admin calls.
3. Use Discover to list CPR OpenAI accounts. This operation is read-only.
4. Select one remote account, a local OpenAI group, a concurrency limit and,
   if required, a proxy for the Sub2API-to-CPR hop. CPR still owns its own
   outbound account proxy and identity.
5. Confirm provisioning and save. This creates or reuses a CPR group containing
   only the selected account, and a client key scoped only to that group.
6. Use a downstream Sub2API key associated with the local group. Call CPR's
   actual model names; this channel does not rewrite model aliases on the wire.

Plain HTTP and private/local CPR hosts must be allowed by the existing
`security.url_allowlist` configuration. A URL such as `127.0.0.1` is relative
to the Sub2API process or container, not to the administrator's browser.
Do not disable a deployment's security policy globally just to connect a remote
gateway; configure the required permitted destinations.

Only dedicated single-account CPR groups and client keys are provisioned.
Discovery never writes to CPR. Deleting the local account does not delete
the remote account or its resources. Admin credentials must never enter
inference requests or ordinary account DTOs. Preserve the Admin Key as a secret.

The selected CPR gateway and remote account are immutable for a local mapping.
Create a new mapping instead of repointing an existing account's history.
Provisioning is serialized within one process. CPR does not provide a
distributed transaction with Sub2API: failed saves may leave a reusable remote
group/key, and concurrent provisioning by separate instances requires care.
Do not manually widen a managed CPR group's membership or its client-key scope.

## Relay Contract

- HTTP and SSE preserve the original business body, encoded request body,
  response bytes, status, query and end-to-end headers. Sub2API authentication
  is replaced with the CPR client key; hop-by-hop headers, cookies, forwarded
  client identity headers and transport-generated fields are not copied.
- The integration branches cover Responses, Chat Completions, Messages, Images,
  input/count-token and alpha-search handlers. Existing route aliases use their
  original path. This is not an unrestricted catch-all for every CPR admin,
  realtime, video or future endpoint.
- CPR-only groups relay `/models` from CPR. Mixed-provider groups keep the
  existing local model catalog behavior.
- WebSocket uses Ranxi's ingress authorization, per-turn concurrency,
  pricing context and connection lifecycle. Frames remain unchanged. A second
  simultaneous generation on the same connection is rejected; control frames
  do not acquire another generation slot.
- There is no HTTP retry/failover after contacting CPR. A truncated upstream
  stream is not converted into a fabricated successful completion.
- Existing turn-state headers/fields pass unchanged. This code does not create,
  save, replay or substitute turn-state. Normal response/session affinity is
  still part of the host scheduler.
- Local authentication, tenant/group/model permissions, admission and body
  limits remain active. Invalid or duplicate JSON keys are rejected instead of
  letting local admission and CPR execute different interpretations.

## Billing and Timing

Usage is observed separately from forwarded bytes and is submitted to Ranxi's
existing billing path, including its durable financial journal/deduplication.
There is no second balance-debit implementation in this adapter.

For CPR, an explicitly requested priority/Fast tier remains billable as priority
when the response reports the non-authoritative default tier, matching the
previous Codex relay contract. Explicit flex can lower the billed tier; a
request without Fast does not gain Fast merely because the response reports it.
Normal API-key channels retain their existing tier rules.

Cached input, cache creation and ordinary input remain disjoint billing buckets.
Long-context billing is controlled by the existing group/account settings.
Image counts and sizes are observed with the native image accounting helper;
no image body is rewritten or downloaded by this integration.

Ranxi's semantic/visible TTFT classifier and WebSocket turn timing are retained.
In particular, a later WS delta without a response ID can retain Ranxi's null
TTFT result. This adapter does not invent a first-token timestamp.

## Quota Display

Remote quota is a read-only observation, not proof of a successful request,
and an unavailable observation is never presented as zero usage.
The display caches queries for ten minutes and supports explicit query refresh.
It distinguishes the last successful query from CPR's reported quota age.
A failed query can display an earlier snapshot as stale.
Remote quota snapshots never overwrite the host's OAuth scheduling windows.
This integration does not reset quota, revive accounts or rotate accounts.

## Limits and Verification

Metering keeps at most 4 MiB per decoded JSON observation/SSE event and a
64 MiB decoded observation budget for a compressed response. Original bytes
are still relayed beyond those bounds. Missing or limited usage is explicitly
marked unavailable and logged, rather than being asserted as known zero.
It cannot be reconstructed from opaque bytes after the request finishes.
Therefore this preview does **not** guarantee lossless accounting for oversized
payloads, unsupported encodings, malformed usage, or streams without final usage.
Operators must reconcile these cases before commercial deployment.

Mock-backed tests cover one-to-one provisioning, credential separation,
compressed HTTP byte preservation, upstream errors without replay, cumulative
usage, image accounting, tier pricing, native TTFT and WebSocket round handling.
The frontend has dedicated CPR modal tests and uses the existing UI patterns.
An isolated real-application test is supplied below. Its upstream is synthetic;
passing it is not proof of compatibility with every deployed CPR version.

Build and run the complete application fixture on a Linux Docker host:

```sh
cd frontend
corepack pnpm@9.15.9 install --frozen-lockfile
corepack pnpm@9.15.9 run build
cd ../backend
go test -tags=unit -p 1 ./internal/pkg/rawproxy ./internal/pkg/httputil ./internal/service/openai_ws_v2
go test -tags=unit -p 1 ./internal/service -run TestCPR
CGO_ENABLED=0 go build -tags=embed -o ../.fixture-bin/sub2api ./cmd/server
CGO_ENABLED=0 go build -tags=fullappfixture -o ../.fixture-bin/fullapp-fixture ./cmd/fullapp-fixture
cd ..
FULLAPP_BIN="$PWD/.fixture-bin/sub2api" \
FULLAPP_FIXTURE_BIN="$PWD/.fixture-bin/fullapp-fixture" \
python3 deploy/traffic/test_cpr_fusion.py
```

The runner creates a random internal Docker network, fresh PostgreSQL/Redis,
synthetic CPR and this application. It publishes no host port, uses no real
account/key, and removes its containers/network in `finally`. The test asserts
SSE concurrency and WebSocket turns, exact usage/dedup row counts, positive
balance debit and released concurrency/queue slots. It does not modify any
existing database, service or account. `DOCKER_BIN` and `DOCKER_HOST` may select
an isolated build daemon.

Real CPR admin provisioning, real upstream inference, long-running load,
all media combinations and cross-instance provisioning still require a
separate staging acceptance run. No production deployment is part of this
source publication. Build-time verification results are recorded separately.

## Verification Record: 2026-10-29

The source-preview verification performed before publication:

| Check | Result |
| --- | --- |
| Full rawproxy, httputil and openai_ws_v2 package tests | Passed |
| CPR service/provisioning and transport contract tests | Passed |
| Selected host billing, service-tier, TTFT, usage and updater regressions | Passed |
| CPR account modal tests | 3 passed |
| Frontend i18n validation, vue-tsc and Vite build | Passed |
| Embedded application and guarded fixture builds | Passed |
| Fresh real application, PostgreSQL and Redis with synthetic CPR | Passed |

The final isolated application run completed 3 SSE requests with concurrency
limited to 2, followed by 2 turns on one WebSocket connection. It recorded
exactly 5 usage rows and 5 deduplication rows, 35 input tokens and 10 output
tokens. The synthetic balance changed from 10 to 9.99983. All observed user,
account and API-key concurrency/queue slots returned to zero. Cleanup completed.

These are selected regressions and synthetic end-to-end checks, not the entire
repository test suite, a real-account CPR test, or a production load test.
GitHub Actions results are separate from this pre-publication verification.

## Security and CI Hardening

The follow-up source update on 2026-10-29 upgrades `vue-i18n` and
`@intlify/message-compiler` to `11.4.14` for `GHSA-83rm-6c8r-g728`, and
`golang.org/x/net` to `v0.62.0` for `GO-2026-6860`, including the required
companion `golang.org/x/*` dependency updates. Frontend CI jobs use Node 22.
No new vulnerability-audit exceptions were added.

The update also restores tracked deployment scripts' executable bits, mounts
the CPR account dialog only while it is open, updates the route-order
regression to include CPR admission without removing authorization checks,
and addresses checked-close/unused-code lint findings. Raw relay destinations
remain bound to the configured, policy-validated CPR origin; regression tests
reject attempts to substitute an inbound authority and verify redirects are
not followed. The two corresponding taint-analysis annotations document this
boundary locally, not a global SSRF-check exclusion.

The Linux application fixture is compiled with `CGO_ENABLED=0` so artifacts
built on a glibc host can start in its Alpine runtime containers.

The production-dependency audit reports **0 high, 0 critical, 13 moderate and
2 low** advisories. The Go scan reports **0 reachable vulnerabilities**, with
2 module-level advisories outside the application's detected call paths.
These residual findings are not a claim that every dependency is vulnerability
free. The frontend critical suite passes **109 files / 1783 tests**, including
the added interpolation/srcdoc regression checks.
