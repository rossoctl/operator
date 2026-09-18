# Token Broker Architecture

## Purpose

The Token Broker enables **HITL (Human-in-the-Loop) Authorization**: when an agent needs access to a resource that requires OAuth permissions not already present in its token, the Token Broker brings the user into the loop via an OAuth 2.0 PKCE flow and caches the resulting access token for the session.

Rossoctl supports three route types for outbound agent requests:
- **Passthrough** — forward the token as-is
- **Token Exchange** — use a subset of permissions already in the agent's token
- **Token Broker** — obtain additional permissions with the user's help (this service)

---

## System Context

```
┌─────────────┐         ┌──────────────┐         ┌──────────────────┐
│   Backend   │◄────────┤ Token Broker ├────────►│ Resource Server  │
│             │  Events │              │ Discovery│ (MCP/LLM/API/..) │
└─────────────┘         └──────┬───────┘         └──────────────────┘
                               │ Token Cache
                               │ Session Store
                               ▼
                        ┌──────────────┐
                        │  In-Memory   │
                        └──────────────┘
```

The Token Broker is a cluster-shared HTTP service running in its own pod — it is not a
sidecar. The binary ships inside the operator image and is selected with
`command: [/token-broker]`; it is installed by the operator Helm chart behind
`tokenBroker.enabled`. See `README.md` for build and deployment details.

- **AuthBridge sidecar** calls `POST /sessions/token` to obtain a token for a resource server. This call blocks until the OAuth flow completes.
- **Backend** creates sessions, long-polls for events (`POST /sessions/broker-events`), and ends sessions (`POST /sessions/end`).
- **OAuth provider** redirects the user's browser to `GET /oauth/callback` after consent.
- **Resource servers** are only contacted for `.well-known/oauth-protected-resource` metadata discovery.

---

## Repository Structure

Paths are relative to the `github.com/rossoctl/operator` module root.

```
cmd/token-broker/
  main.go              # Service bootstrap, configuration, HTTP server, graceful shutdown

internal/tokenbroker/
  api/                 # HTTP handlers and route registration
  auth/                # JWT claim extraction (without signature validation)
  cache/               # Token cache: per-(session_key, resource_url), JWT expiry parsing
  core/                # Interfaces, session/transaction types, TokenBroker orchestrator
  oauth/               # OAuth client (PKCE gen, URL building, token exchange) + endpoint discovery
  session/             # SessionManager lifecycle, semaphore, event/token waiter channels

pkg/oauth/             # PKCE: GeneratePKCEChallenge(), GenerateState()
```

Note `cmd/main.go` (no subdirectory) is the **controller-manager** entrypoint, not
this service.

---

## Package Responsibilities

### `cmd/token-broker/main.go`
Service entry point. Reads environment variables, constructs components, wires routes, runs HTTP server, handles graceful shutdown.

### `internal/tokenbroker/core`
- **`interfaces.go`**: `SessionStore`, `TokenCache`, `OAuthDiscoverer`, `Clock`, `Semaphore` interfaces; `Session`, `OAuthTransaction`, `Event`, `TokenResult` types.
- **`broker.go`**: `TokenBroker` — coordinates the full token acquisition flow: cache check → semaphore → OAuth discovery → PKCE generation → auth URL → event → wait for callback → token exchange → cache → unblock waiters.

### `internal/tokenbroker/api`
HTTP handlers. Registers routes on a chi router. Two error formats: flat for the AuthBridge API (`/sessions/token`), nested for the Backend API (all other endpoints).

### `internal/tokenbroker/oauth`
- **`discovery.go`**: `Discoverer` — calls `GET /.well-known/oauth-protected-resource` on the resource server; supports global and per-resource config overrides that skip discovery.
- **`client.go`**: `Client` — `BuildAuthorizationURL()` constructs the OAuth redirect URL with PKCE parameters; `ExchangeToken()` calls the OAuth provider's token endpoint directly.

### `internal/tokenbroker/session`
- **`manager.go`**: `SessionManager` — creates, validates, retrieves, and expires sessions; owns idle timeout timers; notifies token and event waiters on session end.
- **`semaphore.go`**: `SimpleSemaphore` — channel-based semaphore used to enforce one active OAuth flow per session.

### `internal/tokenbroker/cache`
- **`token_cache.go`**: `TokenCache` — stores access tokens keyed by `(session_key, resource_url)`; checks expiry and near-expiry (< 5 min) on retrieval.
- **`jwt_parser.go`**: Extracts the `exp` claim from a JWT to derive cache TTL. Non-JWT tokens are treated as long-lived (1 year).

### `internal/tokenbroker/auth`
JWT claim extraction (`sub`, `session_uid`/`jti`) without signature validation — used as a fallback when `JWT_JWKS_URL` is not configured. When it is configured, signature validation is handled by the `authlib` JWKS verifier.

### `pkg/oauth`
`GeneratePKCEChallenge()` — generates a random code verifier and its SHA256/base64url challenge (RFC 7636 S256 method).
`GenerateState()` — generates a cryptographically random state nonce.

---

## Token Acquisition Flow

When `POST /sessions/token` is received and no cached token exists:

1. Validate session ownership (user in JWT must own the session).
2. Check token cache — return immediately if found.
3. Acquire per-session semaphore (only one OAuth flow per session at a time).
4. Recheck token cache (another request may have completed while waiting).
5. Discover OAuth endpoints from resource server's `.well-known/oauth-protected-resource` (or use configured overrides).
6. Generate PKCE (`code_verifier`, `code_challenge`) and state (`sessionKey.nonce`) at the Token Broker.
7. Build authorization URL with Token Broker's `callback_url`, PKCE, and discovered scopes.
8. Publish `oauth_url_ready` event — Backend receives this via long-poll and opens the OAuth consent window.
9. Wait for the OAuth provider to call `GET /oauth/callback` with `code` and `state`.
10. Exchange `code` + `code_verifier` directly with the OAuth provider's token endpoint.
11. Cache the resulting access token.
12. Unblock any other waiters for the same `(session, resource_url)`.
13. Return token to the AuthBridge caller.

### Key security properties
- `code_verifier` never leaves the Token Broker.
- `client_secret` is held only by the Token Broker.
- Authorization code goes directly from OAuth provider to Token Broker (not through Backend).
- Session ownership is validated before the cache is checked — prevents session hijacking.

---

## Concurrency Model

- **Per-session semaphore** (capacity 1): at most one OAuth flow runs per session at a time.
- **Double-checked locking**: cache is checked before *and* after semaphore acquisition.
- **`session.Done` channel**: closed when a session ends; unblocks any goroutine waiting on OAuth completion so it returns promptly with a "session ended" error.
- **`session.EventWaiters` channel** (buffered, cap 1): delivers one event per long-poll response.

---

## Session Lifecycle

```
1. POST /sessions            → session created, timer not yet started
2. POST /sessions/broker-events  → timer reset while Backend is connected
3. request completes         → idle timer starts (default 60s)
4. next poll arrives         → timer reset
5. timer fires               → session expired, all waiters unblocked
   OR
   POST /sessions/end        → session terminated explicitly
```

---

## Error Response Formats

Two formats are used to match the consumers:

**AuthBridge API** (`POST /sessions/token`) — flat:
```json
{ "code": "error_code", "message": "description" }
```

**Backend API** (all other endpoints) — nested:
```json
{ "error": { "code": "error_code", "message": "description" } }
```
