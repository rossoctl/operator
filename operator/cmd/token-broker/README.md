# Token Broker Service

The Token Broker is a Rossoctl service that enables **HITL (Human-in-the-Loop) Authorization** — allowing agents to obtain additional user permissions at runtime, beyond those provided when the task was submitted.

## Overview

Rossoctl supports three route types for outbound agent requests:

- **Passthrough** — forward the token as-is
- **Token Exchange** — use a subset of the permissions already in the agent's token
- **Token Broker** — obtain additional permissions with the user's help (this service)

The Token Broker handles the third case. When an agent needs to access an external or internal resource that requires permissions not already present in its token, the Token Broker brings the user into the loop via an OAuth 2.0 PKCE flow — obtaining just-in-time, user-scoped credentials. Neither the agent nor Rossoctl ever holds the user's credentials; the Token Broker only caches the resulting access token and controls which agents (those in the same user session) may use it.

The Token Broker:
- **Acts as the OAuth client** with OAuth credentials (client_id, client_secret)
- **Generates PKCE** (code_verifier, code_challenge) for each OAuth flow
- **Exchanges tokens directly** with OAuth provider (not through the resource server)
- Manages OAuth sessions with timeout and lifecycle management
- Caches tokens per (session, resource_url) with JWT expiry parsing
- Discovers OAuth endpoints and scopes from resource servers' `.well-known` metadata
- Coordinates OAuth flows with the Backend via long-polling events

Resources that can be brokered include MCP servers, LLM APIs, direct REST APIs, and other agents — anything that accepts an OAuth bearer token.

## Architecture

```
┌─────────────┐         ┌──────────────┐         ┌──────────────────┐
│   Backend   │◄────────┤ Token Broker ├────────►│ Resource Server  │
│             │  Events │              │ Discovery│ (MCP/LLM/API/..) │
└─────────────┘         └──────┬───────┘         └──────────────────┘
                               │
                               │ Token Cache
                               │ Session Store
                               ▼
                        ┌──────────────┐
                        │   Storage    │
                        │  (In-Memory) │
                        └──────────────┘
```

## Building

The binary ships inside the operator image alongside `manager` and
`bundle-service`; `ENTRYPOINT` is `/manager`, so a Deployment selects this one
with `command: [/token-broker]`.

```bash
# From the operator module
go build -o token-broker ./cmd/token-broker/

# Or the whole image (all three binaries)
docker build -f Dockerfile -t rossoctl-operator:dev .
```

## Deployment

The broker ships inside the operator image and is installed by the operator Helm
chart, gated off by default:

```bash
# Create the OAuth credentials Secret first — the chart never templates
# credentials, so they stay out of values and Helm release state.
kubectl create secret generic github-oauth-credentials -n rossoctl-system \
  --from-literal=client-id=<CLIENT_ID> \
  --from-literal=client-secret=<CLIENT_SECRET>

helm upgrade --install rossoctl-operator charts/operator \
  --namespace rossoctl-system \
  --set tokenBroker.enabled=true
```

Templates live in `charts/operator/templates/tokenbroker/` (ServiceAccount,
Deployment, Service, HTTPRoute); values are under `tokenBroker` in
`charts/operator/values.yaml`.

### OAuth credentials

The chart never creates the Secret, so credentials never pass through values or
Helm release state. Create it however your environment manages secrets — the
`kubectl create secret` above for dev, or an external store (ExternalSecrets,
Vault, SOPS) in production. The Deployment reads it by reference:

| Value | Default | Meaning |
|-------|---------|---------|
| `tokenBroker.oauth.existingSecret` | `github-oauth-credentials` | Secret name |
| `tokenBroker.oauth.clientIdKey` | `client-id` | key holding the client ID |
| `tokenBroker.oauth.clientSecretKey` | `client-secret` | key holding the client secret |

Point these at whatever your secret store produces, e.g.:

```bash
helm upgrade --install rossoctl-operator charts/operator \
  --namespace rossoctl-system \
  --set tokenBroker.enabled=true \
  --set tokenBroker.oauth.existingSecret=my-oauth-creds \
  --set tokenBroker.oauth.clientIdKey=id \
  --set tokenBroker.oauth.clientSecretKey=secret
```

The Secret must exist in the release namespace before the pod starts; without it
the container stays in `CreateContainerConfigError`.

Default deployment settings:

- Namespace: the release namespace
- Deployment / Service name: `token-broker`
- Port: `8190`
- Replicas: **fixed at 1** — sessions and the token cache are in-memory, so
  scaling out requires shared state first. This is deliberately not a chart value.

### The callback route must match

`tokenBroker.oauth.callbackUrl` and `tokenBroker.httpRoute.hostname` must use the
**same host**. The OAuth provider redirects the browser to that URL and the gateway
routes host + `/oauth/callback` to the broker; a mismatch (or an unattached route)
makes the post-consent redirect 404, and the broker then blocks waiting for a
callback that never arrives.

Both default to `token-broker.localtest.me`, which works out of the box on a
kind/dev cluster. Override both for real deployments, or set
`tokenBroker.httpRoute.enabled=false` and route the callback yourself.

### Local development against kind

```bash
./hack/kind-reload-all.sh [cluster-name] [namespace]
# Defaults: cluster=rossoctl, namespace=rossoctl-system
```

Builds the operator image, loads it into kind, and deploys all three services.
The broker is deployed only if `operator/.env` supplies
`GITHUB_OAUTH_CLIENT_ID` and `GITHUB_OAUTH_CLIENT_SECRET`; otherwise it is skipped.

## Configuration

### Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `TOKEN_BROKER_PORT` | `8190` | HTTP server port |
| `OAUTH_CLIENT_ID` | **(required)** | OAuth client ID for the OAuth App |
| `OAUTH_CLIENT_SECRET` | **(required)** | OAuth client secret for the OAuth App |
| `OAUTH_CALLBACK_URL` | `http://localhost:8190/oauth/callback` | URL the OAuth provider redirects to after user consent |
| `TOKEN_BROKER_SESSION_TIMEOUT` | `60s` | Idle session timeout |
| `TOKEN_BROKER_MAX_SESSIONS_PER_USER` | `5` | Max concurrent sessions per user |
| `TOKEN_BROKER_TOKEN_WAIT_TIMEOUT` | `300s` | Max time to wait for OAuth completion |
| `ALLOWED_REDIRECT_HOSTS` | *(none — all permitted)* | Comma-separated permitted hostnames for `backend_session_redirect_url`; set in production |
| `JWT_JWKS_URL` | *(none — validation disabled)* | JWKS endpoint for JWT signature validation; set in production |
| `JWT_ISSUER` | *(none)* | Expected JWT issuer; required when `JWT_JWKS_URL` is set |
| `JWT_AUDIENCE` | *(none)* | Comma-separated expected audiences; optional |
| `OAUTH_AUTHORIZATION_ENDPOINT` | *(discovered)* | Override OAuth authorization endpoint — skips `.well-known` discovery when set together with `OAUTH_TOKEN_ENDPOINT` |
| `OAUTH_TOKEN_ENDPOINT` | *(discovered)* | Override OAuth token endpoint — skips `.well-known` discovery when set together with `OAUTH_AUTHORIZATION_ENDPOINT` |
| `OAUTH_SCOPES_SUPPORTED` | *(discovered)* | Comma-separated global scope list, used only when discovery is skipped |
| `RESOURCE_CONFIG` | *(none)* | Per-resource OAuth config (scopes + endpoint overrides) — see below |
| `RESOURCE_SCOPES` | *(none)* | Per-resource scope override, legacy format — see below |

### Security-critical variables

**`JWT_JWKS_URL`** — when unset, JWT signatures are **not validated** and any caller can forge a token. Always set this in production. The Token Broker logs a WARNING at startup when it is unset.

**`ALLOWED_REDIRECT_HOSTS`** — when unset, `backend_session_redirect_url` accepts any host. Always set this to the hostname(s) of your backend UI in production. The Token Broker logs a WARNING at startup when it is unset.

#### Per-resource OAuth configuration (`RESOURCE_CONFIG`)

`RESOURCE_CONFIG` is a JSON map of resource URL to per-resource OAuth overrides. It takes precedence over `RESOURCE_SCOPES` and over `.well-known` discovery for matching resources.

Each entry supports:
- `scopes` — list of OAuth scopes to request (replaces discovered scopes)
- `authorization_endpoint` — override the authorization endpoint
- `token_endpoint` — override the token endpoint

```bash
export RESOURCE_CONFIG='{
  "http://mcp-server.example.com": {"scopes": ["read:user", "user:email"]},
  "http://llm-api.example.com": {
    "scopes": ["model:read"],
    "authorization_endpoint": "https://idp.example.com/oauth/authorize",
    "token_endpoint": "https://idp.example.com/oauth/token"
  }
}'
```

The resource URL must match the value in the `X-Server-Url` header exactly (no trailing-slash normalization).

#### Per-resource scope override, legacy (`RESOURCE_SCOPES`)

Deprecated in favor of `RESOURCE_CONFIG`. Overrides scopes only, no endpoint overrides.

Format: comma-separated `<resource-url>=<scope1> <scope2>` entries.

```bash
export RESOURCE_SCOPES="\
  https://mcp.example.com=read:user user:email,\
  https://mcp2.example.com=read:user user:email"
```

### Example Configuration

```bash
export TOKEN_BROKER_PORT=8190
export OAUTH_CLIENT_ID=Ov23liXXXXXXXXXXXXXX
export OAUTH_CLIENT_SECRET=your_oauth_app_secret
export OAUTH_CALLBACK_URL=https://token-broker.example.com/oauth/callback
export ALLOWED_REDIRECT_HOSTS=app.example.com
export JWT_JWKS_URL=https://keycloak.example.com/realms/rossoctl/protocol/openid-connect/certs
export JWT_ISSUER=https://keycloak.example.com/realms/rossoctl
export TOKEN_BROKER_SESSION_TIMEOUT=60s
export TOKEN_BROKER_MAX_SESSIONS_PER_USER=5
export TOKEN_BROKER_TOKEN_WAIT_TIMEOUT=300s

# Optional: per-resource OAuth config (scopes + endpoint overrides)
export RESOURCE_CONFIG='{"http://mcp.example.com": {"scopes": ["read:user", "user:email"]}}'
```

## Running

### Local Development

```bash
# Set required configuration
export OAUTH_CLIENT_ID=Ov23liXXXXXXXXXXXXXX
export OAUTH_CLIENT_SECRET=your_secret

# Run the service
./token-broker
```

The service will start on port 8190 by default and log to stdout in JSON format.

### Docker

```bash
docker build -f Dockerfile -t rossoctl-operator:dev .
docker run -p 8190:8190 \
  --entrypoint /token-broker \
  -e OAUTH_CLIENT_ID=Ov23liXXXXXXXXXXXXXX \
  -e OAUTH_CLIENT_SECRET=your_secret \
  rossoctl-operator:dev
```

## API Endpoints

### POST /sessions

Create a new OAuth session.

**Headers:**
- `Authorization`: Bearer JWT token (required)
  - JWT claims: `sub` (user_id), `session_uid` (session_key) or `jti` (session_key, fallback)

**Body:**
```json
{ "backend_session_redirect_url": "https://app.example.com/oauth-complete" }
```

**Response:** `201 Created` (empty body)

**Errors:**
- `400` - Missing/invalid request or redirect URL host not in `ALLOWED_REDIRECT_HOSTS`
- `401` - Invalid JWT
- `429` - Max sessions per user exceeded

---

### POST /sessions/token

Request a token for a resource (MCP server, LLM API, etc.). Blocks until token is available or timeout.

**Headers:**
- `Authorization`: Bearer JWT token (required)
- `X-Server-Url`: resource base URL (required)

**Response:** `200 OK`
```json
{ "token": "access-token" }
```

**Errors:**
- `401` - Invalid JWT or session
- `408` - Timeout waiting for OAuth completion
- `503` - OAuth flow failed

---

### POST /sessions/broker-events

Long-poll for OAuth events from the Token Broker.

**Headers:**
- `Authorization`: Bearer JWT token (required)

**Response:** `200 OK`

Authorization URL ready (Backend should open OAuth consent window):
```json
{
  "type": "oauth_url_ready",
  "auth_url": "https://github.com/login/oauth/authorize?client_id=...&code_challenge=...&state=..."
}
```

Or error event:
```json
{
  "type": "error",
  "message": "Session expired",
  "code": "session_expired"
}
```

---

### POST /sessions/end

Terminate a session and release all resources.

**Headers:**
- `Authorization`: Bearer JWT token (required)

**Response:** `200 OK`

---

### GET /oauth/callback

OAuth provider callback endpoint. Receives the authorization code from the OAuth provider after the user consents.

**Query Parameters:**
- `code`: Authorization code (required)
- `state`: OAuth state parameter (required, format: `sessionKey.nonce`)

**Response:** `302 Found` — redirects user to `backend_session_redirect_url`

**Note:** Called by the OAuth provider, not by the Backend or AuthBridge.

---

## Testing

### Manual Testing with curl

1. **Generate session key and JWT token:**
```bash
SESSION_KEY=$(uuidgen | tr '[:upper:]' '[:lower:]')
USER_ID="test-user"
JWT_HEADER='{"alg":"none","typ":"JWT"}'
JWT_PAYLOAD="{\"sub\":\"$USER_ID\",\"session_uid\":\"$SESSION_KEY\"}"
JWT_TOKEN=$(echo -n "$JWT_HEADER" | base64 | tr -d '=' | tr '/+' '_-').$(echo -n "$JWT_PAYLOAD" | base64 | tr -d '=' | tr '/+' '_-').
```

2. **Create a session:**
```bash
curl -X POST http://localhost:8190/sessions \
  -H "Authorization: Bearer $JWT_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"backend_session_redirect_url": "http://localhost:3000/oauth-complete"}'
```

3. **Start event long-poll (in another terminal):**
```bash
curl -X POST "http://localhost:8190/sessions/broker-events" \
  -H "Authorization: Bearer $JWT_TOKEN"
```

4. **Request a token (in another terminal):**
```bash
curl -X POST "http://localhost:8190/sessions/token" \
  -H "Authorization: Bearer $JWT_TOKEN" \
  -H "X-Server-Url: http://localhost:8082"
```

5. **End session:**
```bash
curl -X POST "http://localhost:8190/sessions/end" \
  -H "Authorization: Bearer $JWT_TOKEN"
```

## Logging

The Token Broker logs to stdout in JSON format for Kubernetes compatibility.

**Log Levels:**
- `INFO` - Normal operations (session creation, token acquisition, OAuth flows)
- `DEBUG` - Detailed flow information (full auth URLs, cache hits, semaphore acquisition)
- `WARN` - Recoverable issues and misconfiguration (missing JWT_JWKS_URL, missing ALLOWED_REDIRECT_HOSTS)
- `ERROR` - Errors requiring attention (OAuth failures, internal errors)

## Session Lifecycle

```
1. Backend creates session → Session created (active)
2. Backend polls /broker-events → Timer reset
3. Backend disconnects → Timer starts (60s)
4. Backend reconnects → Timer reset
5. Timer expires OR Backend calls /end → Session terminated
```

## Token Caching

Tokens are cached per `(session_key, resource_url)`:
- JWT tokens: Expiry parsed from `exp` claim
- Non-JWT tokens: Treated as long-lived (1 year)
- Near-expiry tokens (< 5 min): Treated as expired
- Only agents in the same user session may use a cached token

## Concurrency Model

- **Per-session semaphore**: Only one OAuth flow per session at a time
- **Double-checked locking**: Check cache before and after semaphore acquisition
- **Session isolation**: Different sessions run OAuth flows independently

## Error Handling

Two error response formats are used depending on the caller:

**AuthBridge API** (`POST /sessions/token`) — flat format:
```json
{ "code": "error_code", "message": "Human-readable message" }
```

**Backend API** (`POST /sessions`, `/sessions/broker-events`, `/sessions/end`) — nested format:
```json
{ "error": { "code": "error_code", "message": "Human-readable message" } }
```

**Error Codes:**
- `invalid_request` - Missing or invalid parameters
- `unauthorized` - Invalid session or user mismatch
- `session_not_found` - Session does not exist
- `session_expired` - Session timed out
- `too_many_sessions` - User exceeded max sessions
- `timeout` - OAuth flow did not complete in time
- `oauth_failed` - OAuth discovery or exchange failed
- `internal_error` - Internal server error

## Security Considerations

- Session keys are UUID v4 (cryptographically random)
- User ID validation on every request; session ownership strictly enforced
- **JWT signature validation** via JWKS when `JWT_JWKS_URL` is configured (recommended for production)
- **Redirect host validation** via `ALLOWED_REDIRECT_HOSTS` (recommended for production)
- **Token Broker acts as OAuth client** with client credentials
- **PKCE generated by Token Broker** (code_verifier never leaves Token Broker)
- **Token exchange happens directly** between Token Broker and OAuth provider
- `code_verifier` never exposed to Backend or resource server
- `client_secret` held only by Token Broker (not the resource server)
- Tokens stored in memory only (no persistence)
- Only agents belonging to the same user session may use a cached token

## Troubleshooting

### Session expires too quickly
Increase `TOKEN_BROKER_SESSION_TIMEOUT`:
```bash
export TOKEN_BROKER_SESSION_TIMEOUT=120s
```

### OAuth flow times out
Increase `TOKEN_BROKER_TOKEN_WAIT_TIMEOUT`:
```bash
export TOKEN_BROKER_TOKEN_WAIT_TIMEOUT=600s
```

### Too many sessions error
Increase `TOKEN_BROKER_MAX_SESSIONS_PER_USER`:
```bash
export TOKEN_BROKER_MAX_SESSIONS_PER_USER=10
```

### Token not cached
Check logs for JWT parsing errors. Non-JWT tokens are treated as long-lived.

### Redirect URL rejected (400)
The `backend_session_redirect_url` host is not in `ALLOWED_REDIRECT_HOSTS`. Add the backend hostname to the env var.

## Development

### Project Structure

The broker lives in the `github.com/rossoctl/operator` module:

```
cmd/token-broker/
  main.go          # Service bootstrap and configuration

internal/tokenbroker/
  api/             # HTTP handlers
  cache/           # Token cache with JWT expiry parsing
  core/            # Token acquisition orchestration and interfaces
  oauth/           # OAuth client (PKCE, URL building, token exchange) and discovery
  session/         # Session lifecycle, semaphore, event coordination
  auth/            # JWT claim extraction

pkg/oauth/         # PKCE challenge generation (GeneratePKCEChallenge, GenerateState)
```

### Adding New Features

1. Update interfaces in `internal/tokenbroker/core/interfaces.go`
2. Implement in the appropriate package
3. Add unit tests
4. Update this README

## License

See LICENSE file in repository root.
