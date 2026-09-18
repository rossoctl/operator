# API Specification: Token Broker Service Interfaces

The Token Broker enables HITL (Human-in-the-Loop) Authorization: when an agent needs access to a resource (MCP server, LLM API, direct REST API, another agent) that requires permissions beyond those already in its token, the Token Broker brings the user into the loop via an OAuth 2.0 consent flow.

This document provides detailed API specifications for the interfaces between:
1. **Token Broker Service** ↔ **AuthBridge Sidecar** (inside AI Agent Pod)
2. **Token Broker Service** ↔ **Backend**

## Table of Contents

- [Overview](#overview)
- [Token Broker Service API](#token-broker-service-api)
- [Interface 1: Token Broker ↔ AuthBridge Sidecar](#interface-1-token-broker--authbridge-sidecar)
- [Interface 2: Token Broker ↔ Backend](#interface-2-token-broker--backend)
- [Error Responses](#error-responses)
- [Headers Reference](#headers-reference)

---

## Overview

The Token Broker Service is a centralized OAuth session and token management service that coordinates OAuth flows between the Backend (user-facing) and the AuthBridge Sidecar (token injection).

### Architecture Context

```
┌─────────────┐         ┌──────────────┐         ┌──────────────────┐
│   Backend   │◄────────┤ Token Broker ├────────►│ Resource Server  │
│             │  Events │   Service    │Discovery│ (MCP/LLM/API/..) │
└──────┬──────┘         └──────┬───────┘         └──────────────────┘
       │                       │
       │ X-OAuth-Session-Key   │ Token Cache
       │                       │ Session Store
       ▼                       ▼
┌──────────────────────────────────────────────┐
│              AI Agent Pod                    │
│  ┌─────────┐  ┌───────────┐  ┌───────────┐ │
│  │  Envoy  │  │AuthBridge │  │ AI Agent  │ │
│  │ Sidecar │  │  Sidecar  │  │ Container │ │
│  │         │  │           │  │           │ │
│  │Inbound: │  │ ext_proc  │  │Listens on │ │
│  │ 15124   │◄─┤  gRPC     │  │127.0.0.1: │ │
│  │         │  │  :9090    │  │  8186     │ │
│  │Outbound:│  │           │  │           │ │
│  │ 15123   │◄─┤ Calls     │  │           │ │
│  │         │  │ Token     │  │           │ │
│  │         │  │ Broker    │  │           │ │
│  └─────────┘  └───────────┘  └───────────┘ │
└──────────────────────────────────────────────┘
```

---

## Token Broker Service API

**Base URL**: `http://token-broker.<namespace>.svc.cluster.local:8190` (Service name: `token-broker`)

**Service Port**: 8190 (ClusterIP)

**Implementation**: [`internal/tokenbroker/api/handlers.go`](../../internal/tokenbroker/api/handlers.go)

---

## Interface 1: Token Broker ↔ AuthBridge Sidecar

The AuthBridge Sidecar calls the Token Broker to obtain OAuth tokens for resource server requests.

### 1.1 Request Token

**Endpoint**: `POST /sessions/token`

**Purpose**: Request an OAuth token for a specific resource server using JWT-based authentication. This call blocks until the token is available or the request times out.

**Called By**: AuthBridge Sidecar (from cortex/authbridge)

**Authentication**: JWT Bearer token containing user ID and session key

**Request Headers**:
```
Authorization: Bearer <jwt_token>
X-Server-Url: <resource_url>
```

**JWT Claims** (required):
- `sub` (string): User ID
- `session_uid` (string): Session key (preferred), or `jti` (string) as fallback
- `iss` (string): Issuer (OIDC service URL)
- `aud` (string): Audience
- `exp` (number): Expiration timestamp
- `iat` (number): Issued at timestamp

**Security Note**: JWT signature validation is enabled when `JWT_JWKS_URL` is configured (recommended for production). When unset, the Token Broker logs a WARNING and trusts claims without signature verification — acceptable only in dev/test environments with network policies restricting access.

**Request Body**: None

**Success Response** (200 OK):
```json
{
  "token": "ghp_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"
}
```

**Behavior**:
1. Extracts JWT from `Authorization: Bearer <token>` header
2. Validates JWT (signature, expiry, issuer, audience when `JWT_JWKS_URL` is configured; claims-only otherwise)
3. Extracts `sub` (user ID) and `jti` (session key) from JWT claims
4. Validates session exists and belongs to the specified user
5. Checks token cache for `(user_id, resource_url)` pair
6. If token exists and not expired, returns immediately
7. If token missing:
   - Acquires per-session semaphore (ensures only one OAuth flow per session)
   - Rechecks cache (another request may have obtained token)
   - Initiates OAuth discovery and flow:
     - Calls `GET /.well-known/oauth-protected-resource` on resource server to discover endpoints and scopes
     - **Generates PKCE at Token Broker** (code_verifier, code_challenge)
     - **Builds authorization URL at Token Broker** with PKCE parameters and Token Broker's callback URL
     - Publishes `oauth_url_ready` event to Backend with complete auth URL
     - Waits for OAuth provider to call `GET /oauth/callback` with authorization code
     - **Exchanges code directly with OAuth provider** (not through resource server)
   - Blocks waiting for OAuth completion (up to 300s by default)
8. Returns token when available

**Session Validation Security**: The Token Broker validates session ownership BEFORE checking the token cache. This prevents session hijacking where User B could use User A's stolen session key to obtain User A's cached tokens. The validation order is:
1. Parse JWT and extract claims
2. Validate session exists and belongs to the user from JWT
3. Check token cache
4. If cache miss, initiate OAuth flow

**Error Responses** (flat format):
- `400 Bad Request`: Missing required headers
  ```json
  {
    "code": "invalid_request",
    "message": "Missing X-Server-Url header"
  }
  ```
- `401 Unauthorized`: Invalid JWT or session
  ```json
  {
    "code": "invalid_token",
    "message": "Invalid JWT: missing required claim: sub (user ID)"
  }
  ```
  ```json
  {
    "code": "unauthorized",
    "message": "Invalid session or user mismatch"
  }
  ```
- `408 Request Timeout`: OAuth flow did not complete in time
  ```json
  {
    "code": "timeout",
    "message": "OAuth flow did not complete in time"
  }
  ```
- `503 Service Unavailable`: Failed to obtain token
  ```json
  {
    "code": "oauth_failed",
    "message": "Failed to obtain token: <details>"
  }
  ```

**Timeout**: 300 seconds (5 minutes) - allows time for user to complete OAuth flow

**Implementation**: [`HandleGetToken()`](../../internal/tokenbroker/api/handlers.go)

---


## Interface 2: Token Broker ↔ Backend

The Backend creates sessions, polls for OAuth events, and submits OAuth completion callbacks.

### 2.1 Create Session

**Endpoint**: `POST /sessions`

**Purpose**: Create a new OAuth session for a user with backend redirect URL

**Called By**: Backend (when user submits a task)

**Authentication**: JWT Bearer token containing user ID and session key

**Request Headers**:
```
Authorization: Bearer <jwt_token>
Content-Type: application/json
```

**JWT Claims** (required):
- `sub` (string): User ID
- `session_uid` (string): Session key (preferred), or `jti` (string) as fallback
- `iss` (string): Issuer
- `aud` (string): Audience
- `exp` (number): Expiration timestamp
- `iat` (number): Issued at timestamp

**Security Note**: JWT signature validation is enabled when `JWT_JWKS_URL` is configured (recommended for production). When unset, the Token Broker logs a WARNING and trusts claims without signature verification — acceptable only in dev/test environments with network policies restricting access.

**Request Body**:
```json
{
  "backend_session_redirect_url": "https://backend.example.com/oauth/complete?session=abc123"
}
```

**Success Response** (201 Created):
Empty response body (session key is already known from JWT `jti` claim)

**Behavior**:
1. Extracts JWT from `Authorization: Bearer <token>` header
2. Validates JWT (signature, expiry, issuer, audience when `JWT_JWKS_URL` is configured; claims-only otherwise)
3. Extracts `sub` (user ID) and `jti` (session key) from JWT claims
4. Validates user hasn't exceeded max sessions (default: 5)
5. Creates new session with provided session key
6. Stores `backend_session_redirect_url` for post-OAuth user redirect
7. Starts session timeout timer (default: 60s)

**Error Responses**:
- `400 Bad Request`: Missing or invalid request body
  ```json
  {
    "error": {
      "code": "invalid_request",
      "message": "Missing backend_session_redirect_url in request body"
    }
  }
  ```
- `401 Unauthorized`: Invalid JWT
  ```json
  {
    "error": {
      "code": "invalid_token",
      "message": "Invalid JWT: missing required claim: sub (user ID)"
    }
  }
  ```
- `429 Too Many Requests`: Max sessions exceeded
  ```json
  {
    "error": {
      "code": "too_many_sessions",
      "message": "max sessions per user exceeded"
    }
  }
  ```

**Implementation**: [`HandleCreateSession()`](../../internal/tokenbroker/api/handlers.go)

---

### 2.2 OAuth Callback (NEW)

**Endpoint**: `GET /oauth/callback`

**Purpose**: Receive OAuth authorization code from OAuth provider and redirect user to Backend

**Called By**: OAuth Provider (e.g., GitHub) after user authorizes

**Query Parameters**:
- `code` (string, required): OAuth authorization code
- `state` (string, required): State parameter in format `sessionKey.nonce`

**Success Response** (302 Found):
```
Location: <backend_session_redirect_url>
```

**Behavior**:
1. Extracts `code` and `state` from query parameters
2. Parses state to extract session key (splits on last `.`)
3. Looks up session by session key; validates state matches the stored OAuth transaction
4. Signals the waiting `AcquireToken` goroutine with the authorization code
5. That goroutine exchanges the code directly with the OAuth provider and caches the token
6. Redirects user to Backend's `backend_session_redirect_url` (HTTP 302)

**State Parameter Format**: `sessionKey.nonce`
- Session key may contain `.` characters
- Nonce is base64url encoded (no `.` characters)
- Parsing: Split on LAST `.` to extract session key

**Error Responses**:
- `400 Bad Request`: Missing or invalid parameters
  ```json
  {
    "error": {
      "code": "invalid_request",
      "message": "Missing code or state parameter"
    }
  }
  ```
- `401 Unauthorized`: Invalid state or session not found
  ```json
  {
    "error": {
      "code": "invalid_state",
      "message": "Invalid or expired OAuth state"
    }
  }
  ```
- `503 Service Unavailable`: Token exchange failed
  ```json
  {
    "error": {
      "code": "oauth_failed",
      "message": "Failed to exchange authorization code"
    }
  }
  ```

**Implementation**: [`HandleOAuthCallback()`](../../internal/tokenbroker/api/handlers.go)

---

### 2.3 Poll for Events (Long-Polling)

**Endpoint**: `POST /sessions/broker-events`

**Purpose**: Long-poll for OAuth events (e.g., `oauth_required`, `oauth_url_ready`)

**Called By**: Backend

**Authentication**: JWT Bearer token containing user ID and session key

**Request Headers**:
```
Authorization: Bearer <jwt_token>
```

**JWT Claims** (required):
- `sub` (string): User ID
- `session_uid` (string): Session key (preferred), or `jti` (string) as fallback
- `iss` (string): Issuer
- `aud` (string): Audience
- `exp` (number): Expiration timestamp
- `iat` (number): Issued at timestamp

**Security Note**: JWT signature validation is enabled when `JWT_JWKS_URL` is configured (recommended for production). When unset, the Token Broker logs a WARNING and trusts claims without signature verification — acceptable only in dev/test environments with network policies restricting access.

**Request Body**: None

**Behavior**: Blocks until an event is available or the session closes. Resets the session idle timer while a Backend request is in flight.

**Success Response** (200 OK) - Authorization URL Ready Event:
```json
{
  "type": "oauth_url_ready",
  "auth_url": "https://github.com/login/oauth/authorize?client_id=...&code_challenge=...&state=..."
}
```

**Backend Action**: Open OAuth popup with auth_url

**Success Response** (200 OK) - Error Event:
```json
{
  "type": "error",
  "message": "Session expired",
  "code": "session_expired"
}
```

**Event Processing**:
1. Extracts JWT from `Authorization: Bearer <token>` header
2. Validates JWT (signature, expiry, issuer, audience when `JWT_JWKS_URL` is configured; claims-only otherwise)
3. Extracts `sub` (user ID) and `jti` (session key) from JWT claims
4. Validates session and user
5. Resets session timeout timer (Backend is connected)
6. Waits for event on session's event channel or until session closes
7. Returns event when available or an error event after session close
8. Starts session timeout timer when request completes

**Error Responses**:
- `401 Unauthorized`: Invalid JWT or session
  ```json
  {
    "error": {
      "code": "invalid_token",
      "message": "Invalid JWT: missing required claim: sub (user ID)"
    }
  }
  ```
  ```json
  {
    "error": {
      "code": "unauthorized",
      "message": "Invalid session or user mismatch"
    }
  }
  ```

**Implementation**: [`HandleEvents()`](../../internal/tokenbroker/api/handlers.go)

---

### 2.4 End Session

**Endpoint**: `POST /sessions/end`

**Purpose**: Explicitly end a session and release resources

**Called By**: Backend (when task completes or user disconnects)

**Authentication**: JWT Bearer token containing user ID and session key

**Request Headers**:
```
Authorization: Bearer <jwt_token>
```

**JWT Claims** (required):
- `sub` (string): User ID
- `session_uid` (string): Session key (preferred), or `jti` (string) as fallback
- `iss` (string): Issuer
- `aud` (string): Audience
- `exp` (number): Expiration timestamp
- `iat` (number): Issued at timestamp

**Security Note**: JWT signature validation is enabled when `JWT_JWKS_URL` is configured (recommended for production). When unset, the Token Broker logs a WARNING and trusts claims without signature verification — acceptable only in dev/test environments with network policies restricting access.

**Request Body**: None

**Success Response** (200 OK):
Empty response body

**Behavior**:
1. Extracts JWT from `Authorization: Bearer <token>` header
2. Validates JWT (signature, expiry, issuer, audience when `JWT_JWKS_URL` is configured; claims-only otherwise)
3. Extracts `sub` (user ID) and `session_uid`/`jti` (session key) from JWT claims
4. Validates session and user
5. Marks session as ended
6. Wakes all blocked token waiters with session-ended error
7. Releases session resources
8. Stops session timeout timer

**Error Responses**:
- `401 Unauthorized`: Invalid JWT or session
  ```json
  {
    "error": {
      "code": "invalid_token",
      "message": "Invalid JWT: missing required claim: sub (user ID)"
    }
  }
  ```
  ```json
  {
    "error": {
      "code": "unauthorized",
      "message": "Invalid session or user mismatch"
    }
  }
  ```
- `500 Internal Server Error`: Failed to end session

**Implementation**: [`HandleEndSession()`](../../internal/tokenbroker/api/handlers.go)

---

## Error Responses

All Token Broker error responses follow a consistent JSON format:

```json
{
  "error": {
    "code": "<error_code>",
    "message": "<human_readable_message>"
  }
}
```

### Common Error Codes

| Code | HTTP Status | Description |
|------|-------------|-------------|
| `invalid_request` | 400 | Missing required headers or parameters |
| `unauthorized` | 401 | Invalid session or user mismatch |
| `session_expired` | 401 | Session has expired |
| `session_ended` | 401 | Session was explicitly ended |
| `too_many_sessions` | 429 | User has exceeded max concurrent sessions |
| `timeout` | 408 | OAuth flow did not complete in time |
| `oauth_failed` | 503 | Failed to obtain token from OAuth provider |
| `internal_error` | 500 | Internal server error |

---

## Headers Reference

### Required Headers

| Header | Used By | Description |
|--------|---------|-------------|
| `Authorization` | All endpoints | JWT Bearer token containing `sub` (user ID) and `session_uid`/`jti` (session key) |
| `X-Server-Url` | `POST /sessions/token` | Target resource server URL for token acquisition |

### JWT Authentication

All endpoints use JWT Bearer authentication. The Token Broker extracts `sub` (user ID) and `session_uid` or `jti` (session key) from the JWT claims. When `JWT_JWKS_URL` is configured, full signature/expiry/issuer/audience validation is performed via JWKS. When unset (dev/test mode), claims are trusted without verification and a WARNING is logged at startup.

**JWT Generation**: JWTs must contain the following claims:
- `sub`: User ID
- `session_uid`: Session key (preferred); `jti` is accepted as fallback
- `iss`: Issuer (must match `JWT_ISSUER` when validation is enabled)
- `aud`: Audience (must match `JWT_AUDIENCE` when validation is enabled)
- `iat`: Issued at timestamp
- `exp`: Expiration timestamp

**JWT Validation**: When `JWT_JWKS_URL` is configured, the Token Broker validates signatures, expiry, issuer, and audience via JWKS. When unset (dev/test mode), claims are trusted without verification and a WARNING is logged at startup.

---

## Configuration

The Token Broker Service is configured via environment variables or configuration file:

| Parameter | Default | Description |
|-----------|---------|-------------|
| `TOKEN_BROKER_PORT` | 8190 | HTTP server port |
| `OAUTH_CLIENT_ID` | **(required)** | OAuth client ID |
| `OAUTH_CLIENT_SECRET` | **(required)** | OAuth client secret |
| `OAUTH_CALLBACK_URL` | `http://localhost:8190/oauth/callback` | Token Broker's public OAuth callback URL. The default is dev-only; the chart always sets this from `tokenBroker.oauth.callbackUrl`. |
| `ALLOWED_REDIRECT_HOSTS` | *(none — all permitted, WARNING logged)* | Comma-separated permitted hostnames for `backend_session_redirect_url` |
| `JWT_JWKS_URL` | *(none — validation disabled, WARNING logged)* | JWKS endpoint for JWT signature validation |
| `JWT_ISSUER` | *(none)* | Expected JWT issuer |
| `JWT_AUDIENCE` | *(none)* | Comma-separated expected audiences |
| `TOKEN_BROKER_SESSION_TIMEOUT` | 60s | Idle session timeout after Backend disconnect |
| `TOKEN_BROKER_MAX_SESSIONS_PER_USER` | 5 | Maximum concurrent sessions per user |
| `TOKEN_BROKER_TOKEN_WAIT_TIMEOUT` | 300s | Maximum time to wait for OAuth completion |

**Note**: The `callback_url` must be registered with the OAuth provider (e.g., in GitHub App settings) and must be publicly accessible.

---

## Token Caching

Tokens are cached per `(session_key, resource_url)` pair:

- **Cache Key**: `session_key` + `resource_url`
- **Cache Value**: Access token + expiry time
- **Expiry Logic**:
  - Tokens are treated as expired if less than 5 minutes remain
  - Expired tokens are automatically removed from cache
- **Session isolation**: Only agents belonging to the same user session may use a cached token

**Implementation**: [`internal/tokenbroker/cache/token_cache.go`](../../internal/tokenbroker/cache/token_cache.go)

---

## Session Management

Sessions are managed with the following lifecycle:

1. **Creation**: Backend calls `POST /sessions`
2. **Active**: Backend polls `/events`, AuthBridge requests tokens
3. **Timeout**: Session expires after 60s of Backend inactivity
4. **Explicit End**: Backend calls `POST /sessions/end`

**Session Semaphore**: Each session has a semaphore (capacity: 1) to ensure only one OAuth flow runs at a time per session.

**Implementation**: [`internal/tokenbroker/session/manager.go`](../../internal/tokenbroker/session/manager.go)

---

## OAuth Discovery and Token Exchange Flow (CURRENT ARCHITECTURE)

When a token is needed and not cached, the Token Broker coordinates an OAuth flow where it acts as the OAuth client and receives callbacks directly.

### Current Flow (Token Broker Receives OAuth Callbacks)

1. **Discovery**: Token Broker calls `GET /.well-known/oauth-protected-resource` on resource server
   - Discovers OAuth authorization server base URL
   - Derives authorization_endpoint (base + `/authorize`)
   - Derives token_endpoint (base + `/access_token`)
   - Extracts scopes_supported
2. **PKCE Generation**: **Token Broker generates PKCE** using `pkg/oauth.GeneratePKCEChallenge()`
   - Creates `code_verifier` (random 43-character string)
   - Creates `code_challenge` (SHA256 hash of verifier, base64url encoded)
   - Generates `state` in format `sessionKey.nonce` using `pkg/oauth.GenerateState()`
3. **Storage**: Token Broker stores PKCE, state, and endpoints in session's OAuth transaction
4. **URL Building**: **Token Broker builds authorization URL** with:
   - Token Broker's `client_id`
   - Token Broker's `callback_url` (redirect_uri) - configured OAuth callback endpoint
   - `code_challenge` and `code_challenge_method=S256`
   - `state` (format: `sessionKey.nonce`)
   - `scope` (discovered from resource server)
5. **Event**: Token Broker publishes `oauth_url_ready` event with complete authorization URL
6. **User Flow**: Backend opens OAuth popup for user
7. **Authorization**: User logs in and authorizes the application
8. **Callback**: OAuth provider redirects to Token Broker's `/oauth/callback` with `code` and `state`
9. **Session Lookup**: Token Broker extracts session key from state parameter
10. **Token Exchange**: **Token Broker exchanges directly with OAuth provider**
    - Sends `client_id`, `client_secret`, `code`, `code_verifier`, `redirect_uri` to OAuth provider's token_endpoint
    - OAuth provider validates PKCE and returns access token
    - Token Broker receives access token directly (no resource server intermediary)
11. **Cache**: Token Broker caches received token for `(session_key, resource_url)`
12. **User Redirect**: Token Broker redirects user to Backend's `backend_session_redirect_url` (HTTP 302)
13. **Unblock**: Waiting token requests receive the token

### Why Token Broker Receives OAuth Callbacks

**Architecture Benefits**:

- **Token Broker as OAuth Client**: Registered OAuth client with GitHub, has `client_id` and `client_secret`
- **Direct Callback Receipt**: OAuth provider redirects directly to Token Broker (standard OAuth 2.0 flow)
- **No Code Forwarding**: Authorization code never passes through Backend (more secure)
- **resource server Role**: Only provides metadata (endpoints and scopes via `.well-known`)
- **Centralized OAuth**: Single service handles all OAuth operations
- **Better Security**: `code_verifier` and authorization code never leave Token Broker
- **Simpler Backend**: No OAuth callback handling logic needed
- **Standard OAuth Flow**: Follows OAuth 2.0 best practices

### PKCE Security

**Implementation**:
- **Token Broker generates PKCE** for each OAuth flow
- `code_verifier` stays in Token Broker (never transmitted to user or Backend)
- `code_challenge` sent to OAuth provider in authorization URL
- `code_verifier` sent to OAuth provider during token exchange
- resource server never sees PKCE parameters
- Authorization code received directly by Token Broker (not forwarded through Backend)

**Implementation**: [`internal/tokenbroker/oauth/`](../../internal/tokenbroker/oauth/)

---

## Sequence Diagrams

### Complete OAuth Flow

```mermaid
sequenceDiagram
    participant User
    participant Backend
    participant TokenBroker as Token Broker
    participant AuthBridge
    participant ResourceServer as Resource Server
    participant OAuth as GitHub OAuth

    Backend->>+TokenBroker: POST /sessions<br/>(JWT Bearer auth)<br/>{backend_session_redirect_url}
    TokenBroker-->>-Backend: 201 Created
    
    Backend->>TokenBroker: POST /sessions/broker-events<br/>(JWT Bearer auth, long-poll)
    
    AuthBridge->>+TokenBroker: POST /sessions/token<br/>(JWT Bearer auth)
    TokenBroker->>TokenBroker: Parse JWT (no signature validation)<br/>Validate session ownership<br/>Check cache: miss<br/>Acquire semaphore
    TokenBroker->>+ResourceServer: GET /.well-known/oauth-protected-resource
    ResourceServer-->>-TokenBroker: {authorization_servers, scopes_supported}
    Note over TokenBroker: Token Broker generates PKCE:<br/>code_verifier, code_challenge<br/>state = sessionKey.nonce
    TokenBroker->>TokenBroker: Store PKCE, state, and endpoints<br/>Build auth URL with Token Broker callback
    
    TokenBroker-->>Backend: Event: {type: oauth_url_ready, auth_url}
    Backend->>User: Open OAuth popup
    User->>OAuth: Authorize application
    OAuth-->>TokenBroker: GET /oauth/callback?code=...&state=...
    
    Note over TokenBroker: Extract session key from state<br/>Validate state matches session
    TokenBroker->>+OAuth: POST /token<br/>{client_id, client_secret,<br/>code, code_verifier, redirect_uri}
    OAuth-->>-TokenBroker: {access_token, expires_in}
    TokenBroker->>TokenBroker: Cache token for<br/>(session_key, resource_url)
    TokenBroker-->>User: HTTP 302 Redirect to<br/>backend_session_redirect_url
    
    TokenBroker-->>-AuthBridge: {token}
    User->>Backend: Redirected to session URL
```

### Cached Token (Fast Path)

```mermaid
sequenceDiagram
    participant Backend
    participant TokenBroker as Token Broker
    participant AuthBridge

    Backend->>+TokenBroker: POST /sessions<br/>(JWT Bearer auth)<br/>{backend_session_redirect_url}
    TokenBroker-->>-Backend: 201 Created
    
    AuthBridge->>+TokenBroker: POST /sessions/token<br/>(JWT Bearer auth)
    TokenBroker->>TokenBroker: Parse JWT<br/>Validate session<br/>Check cache: HIT!
    TokenBroker-->>-AuthBridge: {token}
```

---

## Related Documentation

- [Architecture Overview](ARCHITECTURE.md)
- [Deployment and configuration](README.md#deployment)
- [AuthBridge webhook](../../docs/authbridge-webhook.md)

---

## Implementation Notes

### AuthBridge Configuration

The AuthBridge Sidecar is configured to use the Token Broker via routes:

```yaml
routes:
  rules:
    - host: "mcp-server-service"
      action: "broker"
      token_broker_url: "http://token-broker.rossoctl-system.svc.cluster.local:8190"
```

**Configuration**: See `charts/operator/templates/tokenbroker/` and the `tokenBroker` values in `charts/operator/values.yaml`

### Envoy Configuration

Envoy is configured with ext_proc to call AuthBridge:

- **Outbound Listener** (15123): Agent → MCP Server (token injection)
- **Inbound Listener** (15124): Backend → Agent (JWT validation, bypassed in demo)
- **ext_proc Cluster**: AuthBridge at 127.0.0.1:9090

**Configuration**: See `charts/operator/templates/tokenbroker/` and the `tokenBroker` values in `charts/operator/values.yaml`

---

**Last Updated**: 2026-05-23  
**Version**: 1.0  
**Status**: Current Implementation