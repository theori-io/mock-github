# GitHub mock for source-scan tests

`mock-github` serves the GitHub REST subset for an app's repository picker
and repository/ref scan targets, plus GitHub App webhook delivery. It has no
external Go dependencies. The connection UI/client is still unfinished in this checkout; configure
the eventual GitHub client's API base URL to the mock.

## Standalone e2e server

From the repository root:

```sh
go build -o mock-github ./cmd/mock-github
./mock-github -addr 127.0.0.1:8089 \
  -fixture fixtures/example.json
```

The binary prints `{"url":"http://127.0.0.1:8089"}` after binding its listener.
Use `-addr 127.0.0.1:0` for a free port and `-ready-file /tmp/github-ready.json`
to write that JSON atomically. Use a unique ready-file for each process. SIGTERM
and SIGINT trigger graceful shutdown and remove the ready-file. Bind to
`0.0.0.0:8089` when another container needs access. The printed URL uses loopback
for wildcard listeners; containers must use the appropriate service hostname.

```sh
curl http://127.0.0.1:8089/user/repos
curl http://127.0.0.1:8089/repos/acme/demo/branches
curl -o /tmp/source.zip http://127.0.0.1:8089/repos/acme/demo/zipball/main
curl -X POST http://127.0.0.1:8089/__mock/reset
```

Build for another platform without cgo:

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -o mock-github ./cmd/mock-github
```

## Go unit tests

Use a fresh handler per test. `httptest.NewServer(mock)` supports ordinary HTTP
clients; `httptest.NewRecorder()` works for tests that invoke the handler directly.
Within another local Go module, use a `replace` directive to this package's path.

```go
mock, err := mockgithub.New(mockgithub.Config{
    Repositories: []mockgithub.Repository{{Owner: "acme", Name: "demo"}},
})
if err != nil {
    t.Fatal(err)
}
server := httptest.NewServer(mock)
t.Cleanup(server.Close)
// Point the application client's API base URL at server.URL + "/".
// Exercise application behavior, then assert outbound calls:
for _, request := range mock.Requests() {
    t.Log(request.Method, request.Path, request.Status)
}
mock.Reset()
```

Import the package as
`mockgithub "github.com/theori-io/mock-github"`. `Config.Clock`
accepts a function for deterministic timestamps and token-expiration tests.
`Load(config)` replaces the reset baseline atomically; invalid loads preserve
the previous state. `AddStub(rule)` adds a rule that reset removes.

## Supported API

| Endpoint | Behavior |
| --- | --- |
| `GET /user` | Fixture user identity |
| `GET /user/repos`, `GET /orgs/{org}/repos` | Accessible repository selection |
| `GET /repos/{owner}/{repo}`, `GET /repositories/{id}` | Repository metadata with token-scoped access |
| `GET /repos/{owner}/{repo}/branches[/{branch}]` | Branch selection and commit SHA |
| `GET /repos/{owner}/{repo}/commits/{ref}` | Resolve a branch, SHA, or PR head |
| `GET /repos/{owner}/{repo}/pulls[/{number}]` | PR selection and head/base metadata |
| `GET /repos/{owner}/{repo}/zipball[/{ref}]` | ZIP source snapshot |
| `GET /repos/{owner}/{repo}/tarball[/{ref}]` | tar.gz source snapshot |
| `GET /app`, `GET /apps/{slug}` | Authenticated app identity and app profile |
| `GET /app/installations[/{id}]` | GitHub App installation discovery |
| `GET /user/installations` | User installation discovery |
| `GET /orgs/{org}/installation-requests` | Admin-only pending request list |
| `GET /repos/{owner}/{repo}/installation` | Repository installation lookup |
| `POST /app/installations/{id}/access_tokens` | Issue a scoped token |
| `GET /installation/repositories` | Token-scoped repository selection |
| `DELETE /installation/token` | Revoke an installation token |
| `POST /login/oauth/access_token` | Exchange a web flow OAuth code for a user token |

Lists support `page` and `per_page` (default 30, maximum 100) with `Link` headers.
Repository and branch lists sort by name; PR lists sort by number descending,
with `direction=asc`, `state=open|closed|all`, and `base` filters. Branch lists
support `protected=true|false`. Installation repository/user installation lists
return GitHub's `total_count` envelope. Other list filters/sorts are not modeled.
User, repository, and installation responses include defaults for required GitHub
REST schema fields so typed clients can parse minimal fixtures. User/repository
metadata overrides preserve fixture values. Missing repositories/refs and
unimplemented routes return 404.

Refs accept branch names, full commit SHAs, `refs/heads/{branch}`, and
`refs/pull/{number}/head`. Branch names may contain `/`. Omitted archive refs
resolve the default branch. Downloads return archive bytes directly, with one
repository root directory, without a redirect to codeload.github.com. Text files
come from the selected commit's `files` map. ZIP/tar output is deterministic.
This is source download support; Git clone and Git's transport are not modeled.

## Fixtures and authentication

Start with [the example](fixtures/example.json). Set
`user`, `users`, `organizations`, `repositories`, `apps`, `installations`, `webhook_events`, `stubs`,
`max_requests`, and `max_deliveries` in JSON.
Repository fixtures contain `owner`, `name`, optional metadata `data`, `branches`
(name/SHA/protected), `commits` (SHA/files/data), and `pull_requests` (GitHub JSON
objects). Branches and PR heads must reference seeded commits. Commit SHAs must
be 40 lowercase hex characters. File paths must be relative and normalized.
Explicit repository IDs must be unique. Metadata objects can carry additional
GitHub fields consumed by the client.

Each app has a unique positive `id`, unique `slug`, optional GitHub metadata
`data`, opaque `token`, and optional `webhook` receiver. Each installation has an
`app_id`, account, and repository grants. Multiple apps can be installed on the
same organization/repositories. App identity, installation discovery, repository
installation lookup, and token issuance are scoped to the authenticated app;
another app's installation returns 404. `/user/installations` includes all apps;
with organization fixtures it includes installations only for organizations the
authenticated user belongs to.
App responses omit the configured token and webhook secret.

For convenience, omitting `apps` creates app ID 1 with slug `mock-app`. An omitted
installation/event `app_id` selects the sole app; multiple apps require explicit
IDs and distinct nonempty app tokens.

Authentication is optional by default for a single app. `token` configures a
user/PAT credential; `apps[].token` configures each app's opaque JWT substitute.
The legacy top-level `app_token` works with a single app and must agree with its
token if both are supplied. `GITHUB_MOCK_TOKEN` overrides the command's user token.
Both `Bearer` and `token` Authorization schemes work. App credentials are opaque
fixture values; JWT signature verification and the OAuth browser pages are outside
this mock's scope.

For multiple user actors, keep the default identity in `user` with its `token`,
and add other actors to `users` as `{ "data": { "login": "alice", "id": 3 },
"token": "admin-token" }`. Logins/IDs and credentials must be unique; user and
app credentials must differ. Adding `users` requires a known user credential on
ordinary user/PAT routes. `/user` returns the matching actor. Organization fixtures
contain a unique `login`/positive `id` and a `members` map from fixture user logins
to `member` or `admin`. Roles apply separately to each organization.

`/installation/repositories` and `/installation/token` always require a valid
issued installation token. User/PAT and app credentials cannot substitute for
that exchange, even when other endpoints allow requests without authentication.

Installation token requests accept `repositories` (repository names) or
`repository_ids`. Omission selects all repositories granted by the installation.
Custom token permissions are not modeled. Installation `permissions` are
response metadata; permission-specific failures can be injected with stubs.
Issued tokens expire after one hour. Unknown, expired, revoked, and pre-reset
mock tokens return 401. Out-of-scope
repositories return 404. Issued tokens support repository and installation
endpoints, not `/user`. Tokens use the reserved `ghs_mock_` prefix.

## OAuth code exchange

`POST /login/oauth/access_token` models the web flow's code exchange on the API
host, where clients such as githubkit send it for a non-github.com base URL. Set
`apps[].client_id` and `apps[].client_secret`, then give users codes with the
top-level `oauth_code` (for the default `user`/`token`) or `users[].oauth_code`.
The exchange returns the user's fixture `token` as `access_token`, so it works on
every user route. Codes are single use until the next reset or fixture load, and
are accepted by any app with matching client credentials.

Parameters come from the query string and a JSON or form-encoded body. Responses
are JSON when `Accept` includes `application/json` and form-encoded otherwise.
Like GitHub, failures return 200 with `error` set to
`incorrect_client_credentials` or `bad_verification_code`. App responses include
`client_id` and omit `client_secret`.

## Installation request and approval

Use [the approval fixture](fixtures/installation-approval.json)
to start with no installations. It has `octocat` as an `acme` member
(`member-token`), `alice` as its admin (`admin-token`), and `eve` as an outsider
to `acme` (`outsider-token`), with OAuth codes `member-code`, `admin-code`, and
`outsider-code`. App 7 uses `app-jwt` and client credentials `example-client` /
`example-secret`. Replace its webhook URL with
the application's receiver, then start the binary with this fixture.

```sh
curl -X POST http://127.0.0.1:8089/__mock/orgs/acme/installation-requests \
  -H 'Authorization: Bearer member-token' -H 'Content-Type: application/json' \
  -d '{"app_id":7,"repositories":["acme/demo"]}'
curl http://127.0.0.1:8089/orgs/acme/installation-requests \
  -H 'Authorization: Bearer admin-token'
curl -X POST http://127.0.0.1:8089/__mock/orgs/acme/installation-requests/REQUEST_ID/approve \
  -H 'Authorization: Bearer admin-token'
```

Requesting and approving use mock workflow controls for the installation UI flow;
they do not emulate public GitHub REST creation/approval APIs or browser pages.
These controls always require a fixture user credential. Missing/unknown, app,
and installation credentials return 401. Organization members can request;
listing and approving require the target organization's `admin` role (403 for
other users). A role on another organization does not grant access.

Requests select an existing `app_id` and a nonempty list of unique repository
full names owned by the organization. The app must configure a webhook receiver.
Unknown apps, missing receivers, or invalid repository grants return 422.
One request/installation is allowed per app and organization; duplicates return
409. Requesting returns 201 with a pending request and emits no installation
event. Pending requests do not appear in installation discovery and cannot issue
installation tokens. The admin list is a paginated array of pending requests.

Set `installation_id_start` to choose the first ID for approved installations
(default 1); occupied IDs are skipped. Use distinct ranges when resetting fixtures
against a long-lived client that caches installation tokens.

Approval accepts an empty body or empty JSON object. It atomically creates an
installation with a unique ID and the requested repository grants, marks the
request approved, and removes it from the pending list. Permissions come from
`apps[].data.permissions`, defaulting to the mock's read permissions. The new
installation is immediately available through existing discovery and token APIs.
Approval then automatically sends `X-GitHub-Event: installation` with
`action: created`, the installation, repositories, admin sender, and requester,
signed with the owning app's secret. The receiver can call the mock during
delivery. The response waits for this attempt and returns 201 with `request`,
`installation`, and `delivery`.

A receiver failure leaves the installation approved; inspect `delivery` and use
explicit redelivery to retry that event. Repeated/concurrent approval returns
409 and does not create another installation or webhook. Reset or fixture
replacement clears pending/approved requests and restores seeded installations;
reset also revokes issued tokens and cancels pending webhook attempts. These
workflow endpoints work identically with an in-process handler and the binary.

## Webhook-driven e2e tests

Set the app's `webhook.url` to the application's receiver URL, reachable from the
mock process/container. Set `webhook.secret` to the application's test signing
secret. The example targets `http://127.0.0.1:8000/webhooks/github`; replace that
URL with the actual receiver before running your test. Fixtures can be supplied
at startup or replaced through `PUT /__mock/fixtures`.

```json
{
  "apps": [{
    "id": 7,
    "slug": "example-app",
    "webhook": {
      "url": "http://api:8000/webhooks/github",
      "secret": "test-secret",
      "timeout_ms": 10000
    }
  }],
  "webhook_events": [{
    "name": "installation-created",
    "app_id": 7,
    "event": "installation",
    "payload": { "action": "created", "installation": { "id": 42, "app_id": 7 } }
  }]
}
```

The full example includes `push-main`, `pull-request-opened`, and
`installation-created` events consistent with its seeded repository and app.
Trigger a named fixture or supply an inline event and JSON object payload:

```sh
curl -X POST http://127.0.0.1:8089/__mock/webhooks/deliver \
  -H 'Content-Type: application/json' -d '{"fixture":"push-main"}'
curl -X POST http://127.0.0.1:8089/__mock/webhooks/deliver \
  -H 'Content-Type: application/json' \
  -d '{"app_id":7,"event":"ping","payload":{"zen":"Test webhook"}}'
curl http://127.0.0.1:8089/__mock/webhooks/deliveries
curl -X POST http://127.0.0.1:8089/__mock/webhooks/deliveries/DELIVERY_ID/redeliver
```

Delivery sends POST JSON with `X-GitHub-Event`, a UUID `X-GitHub-Delivery`,
`User-Agent: GitHub-Hookshot/mock-github`, and app installation target headers.
`X-Hub-Signature-256` and legacy `X-Hub-Signature` contain HMAC-SHA256/SHA1 of
the exact transmitted body. An empty secret omits signatures; a deliberately
different secret exercises receiver signature rejection. Payloads are supplied
by tests for manual deliveries, without generated fields or REST state changes.
Approval generates its installation event from the committed state. Update REST
fixtures separately for new commits or other changed installation grants.

Triggering waits for the receiver and returns 201 with a delivery record. A
receiver's 4xx/5xx is stored in `response_status`; transport errors, cancellation,
and timeouts appear in `error`, so 201 alone does not mean the receiver accepted
the event. Records include the payload, exact transmitted `body`, headers,
response body/status, attempt number, timestamp, and duration. Receiver responses
are capped at 64 KiB (`response_truncated` indicates truncation). The default
timeout is 10 seconds; `timeout_ms` can be set up to five minutes. Redirects are
recorded without following them. Manual triggers and installation approval cause
outbound calls; loading fixtures alone does not deliver events.

There are no automatic retries. Explicit redelivery preserves the delivery ID,
exact body, receiver URL, and signing secret, and increments the attempt number
for duplicate-event/deduplication tests. The log retains the last 100 completed
attempts by default (`max_deliveries` overrides this). An evicted or reset ID
cannot be redelivered. Reset and fixture replacement cancel pending deliveries
and clear history; canceled attempts cannot reappear in the new history. Reset
cannot undo work the receiver has already accepted.

Go tests use `DeliverWebhook(ctx, WebhookRequest{Fixture: "push-main"})`,
`RedeliverWebhook(ctx, id)`, and `Deliveries()` on the same handler. Invalid
requests return an error; receiver failures return a delivery record. Cancellation
of the supplied context interrupts delivery.

## Failure scenarios and assertions

Response overrides take precedence over modeled routes. For example:

```json
{
  "method": "GET",
  "path": "/repos/:owner/:repo/branches",
  "times": 1,
  "responses": [{
    "status": 429,
    "headers": { "Retry-After": "1" },
    "body": { "message": "rate limited" }
  }]
}
```

`times: 1` makes only the first matching call fail; later calls use normal
behavior. `times: 0` (default) matches indefinitely. Multiple `responses` are
consumed in order, then the last repeats until `times` is exhausted. `delay_ms`
simulates latency (maximum five minutes). Cancellation interrupts the delay.
Paths support `:parameter` segments and a final `*`; query/header matchers match
subsets, and an optional JSON `body` matcher compares the entire parsed value.
Earlier rules take precedence. Overrides return JSON and can be used for an
explicit additional endpoint without implementing its behavior.

| Control endpoint | Action |
| --- | --- |
| `GET /__mock/health` | Readiness check |
| `GET /__mock/requests` | Completed API calls: method, path, query, headers, body, status |
| `DELETE /__mock/requests` | Clear recorded calls |
| `POST /__mock/reset` | Restore fixtures/stubs, clear tokens/logs, cancel webhooks |
| `PUT /__mock/fixtures` | Replace config and reset baseline |
| `POST /__mock/stubs` | Append a response override |
| `POST /__mock/webhooks/deliver` | Deliver a named fixture or inline event |
| `GET /__mock/webhooks/deliveries` | Inspect completed delivery attempts |
| `POST /__mock/webhooks/deliveries/{id}/redeliver` | Repeat a retained delivery |
| `POST /__mock/orgs/{org}/installation-requests` | Authenticated member submits a request |
| `POST /__mock/orgs/{org}/installation-requests/{id}/approve` | Authenticated org admin approves and emits a webhook |

Control calls are not recorded. The log retains the last 1000 completed calls
unless `max_requests` is set. Authorization/Cookie headers are redacted; bodies
and other headers are recorded as supplied, so use synthetic test data. Request
bodies are limited to 1 MiB. API URLs and pagination links use the incoming host.
Archive compression and response writes happen outside the state lock, so a
stalled client does not block other API calls or control operations.
Fixture/reset/log/stub/manual delivery controls are unauthenticated; organization
workflow controls enforce user credentials and roles. Run the mock on a trusted test network.
Use one instance per parallel scenario and reset after in-flight calls finish.

## Validation

`go test -race ./...` runs HTTP contract tests, source archive extraction checks,
token lifecycle/scope tests, app ownership, signed webhook receiver integration,
request/approval permissions and lifecycle, concurrent approvals, redelivery,
timeouts, reset isolation, concurrent access under the race detector,
and a compiled-binary e2e test. Run `gofmt -l .` and `go vet ./...` for formatting and vet checks.
