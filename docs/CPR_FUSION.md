# CPR Fusion

Version: `2.17.6-cpr.2`, a source preview. No existing service is replaced or
automatically migrated. This integration does not require a modified CPR image.

## Provenance

Base: ranxi2001/sub2api v2.17.6, commit
`a7ba38d873ff97faf3a127a97ab808d8860bb8b8`.

The dedicated CPR channel type and protocol integration were adapted from KlN-4096/sub2api
v0.2.19-klno.1, commit `bdff1129ffbd22947612f83cd09d23bf3fa47f5d`,
under the included LGPL v3 license. Original copyright and
license notices are retained. This is a separately maintained derivative,
not an official release of either upstream.

HTTP/SSE forwarding is based on the previously developed rawproxy transport.
The target integration retains Ranxi's TTFT definition, financial journal,
authorization, tenant boundaries and lifecycle hooks. CPR owns upstream
identity and request transformations. No turn-state generation, storage,
replay or substitution is introduced.

## Client Key Setup

1. Build this checkout, rather than installing a binary from an upstream README
   link. The frontend build requires Node 22 or newer and pnpm 9; the Go toolchain is pinned by
   `backend/go.mod`. The existing Dockerfile can build the complete application.
2. Open Accounts and choose **+ CPR Key**. Save the **global CPR gateway URL**
   once, for example `http://127.0.0.1:18090` or a gateway URL ending in `/v1`.
   It is stored in the local settings database as `cpr_gateway_base_url`.
3. Enter a name and an **existing CPR client API key**, not a CPR Admin Key.
   One local channel represents one client key, which may already address an
   entire account pool. Subsequent keys reuse the saved global URL.
4. Select local OpenAI groups, a local concurrency limit and, if required, a
   proxy for the Sub2API-to-CPR hop. These are local downstream access/admission
   settings, not changes to CPR's permissions or account pool.
5. Save. This creates only a local channel. CPR receives inference through the
   supplied key and continues to select accounts under its existing policy.
6. Use a downstream Sub2API key associated with the local group. Call CPR's
   actual model names; this channel does not rewrite model aliases on the wire.

Plain HTTP and private/local CPR hosts must be allowed by the existing
`security.url_allowlist` configuration. A URL such as `127.0.0.1` is relative
to the Sub2API process or container, not to the administrator's browser.
Do not disable a deployment's security policy globally just to connect a remote
gateway; configure the required permitted destinations.

There is no CPR admin API integration: no account discovery, remote account
binding, group creation, key creation/reveal, or pool membership writes.
Adding, editing or deleting a local channel does not change CPR resources.
Client keys are redacted in ordinary account responses; leaving the field
empty while editing retains the current key. Entering another key rotates it.

The admin-only local endpoints `GET/PUT /api/v1/admin/accounts/cpr/gateway`
read/write `{"base_url":"..."}`. Saving is not a connectivity test and does not
make a request to CPR. Changing an existing destination requires confirmation
in the UI because all CPR keys will be sent to the new trusted destination.
Use an empty string to stop new CPR forwarding; an omitted field is rejected.
The live setting is read once per outgoing connection, without a stale local
cache. Existing requests/WebSocket connections retain their original target.
Storage failures, missing configuration and rejected URL policies fail closed:
there is no account-local URL fallback or fallback to the official OpenAI API.

### Upgrading From cpr.1

The old single-account provisioning workflow was the wrong integration model
and has been removed. Configure the global URL, edit each legacy local CPR
channel and supply the desired existing pool key. The form identifies legacy
bindings and requires an explicit replacement key. Editing removes the retired
local Admin Key, admin URL, per-account URL and remote account ID.
Old keys do not automatically acquire wider pool access. No old remote
groups/keys/accounts are deleted, widened or otherwise migrated. A legacy
channel must not be assumed to represent a whole pool until its key is replaced.

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

## Usage and Quota

The local usage journal/billing still observes inference responses.
Remaining pool quota and upstream account status are managed in CPR. A pool key
must not be presented as the 5h/7d quota of an arbitrarily selected account.
The account cell therefore says to view quota in CPR and makes no quota polling
requests. The usage API returns `cpr: {"mode":"client_key","quota_source":"cpr",
"quota_available":false}` without contacting CPR, even on forced refresh.
Old single-account snapshots are ignored. Unknown quota is not zero or unlimited
quota, and no OAuth scheduling window is overwritten.

## Limits and Verification

Metering keeps at most 4 MiB per decoded JSON observation/SSE event and a
64 MiB decoded observation budget for a compressed response. Original bytes
are still relayed beyond those bounds. Missing or limited usage is explicitly
marked unavailable and logged, rather than being asserted as known zero.
It cannot be reconstructed from opaque bytes after the request finishes.
Therefore this preview does **not** guarantee lossless accounting for oversized
payloads, unsupported encodings, malformed usage, or streams without final usage.
Operators must reconcile these cases before commercial deployment.

Mock-backed tests cover global URL persistence, hot reload, fail-closed reads,
client-key-only credentials, retired binding cleanup, unchanged key permissions,
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

Real upstream inference through existing CPR keys, long-running load,
all media combinations and real multi-key pool behavior still require a
separate staging acceptance run. No production deployment is part of this
source publication. Build-time verification results are recorded separately.

## Verification Record: 2026-10-29

Historical verification of **cpr.1**, before the client-key correction:

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
These historical results do not certify the cpr.2 changes.

## Client-Key Verification: 2026-10-30

The cpr.2 correction was verified in an isolated build environment:

- CPR service/relay, native billing/service-tier/TTFT/credential regressions and
  the complete gateway route test package passed.
- The uncapped backend lint run reported 0 issues.
- All 8 CPR frontend tests, frontend lint, i18n checks, type checks and the
  production frontend build passed.
- The separate frontend critical suite passed 109 files / 1783 tests.
- The real application accepted a client-key-only channel through its admin
  API after saving the shared gateway. Unauthenticated settings access and
  updates without `base_url` were rejected.
- `/v1/models` preserved the synthetic CPR key's model manifest.
- The synthetic CPR observed **zero admin API requests**.
- Three SSE requests and two WebSocket turns produced exactly 5 usage rows,
  5 deduplication rows, 35 input tokens and 10 output tokens. The synthetic
  balance changed from 10 to 9.99983; concurrency was bounded to 2 and all
  observed slots/queues were released.

The disposable fixture database contains synthetic admin onboarding state;
the runner does not accept an agreement on behalf of a real operator or weaken
the production admin/authentication guards. Test containers and their internal
network were removed. These results use no real CPR key or upstream account
and are not a production deployment or a real-account/load acceptance test.

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
