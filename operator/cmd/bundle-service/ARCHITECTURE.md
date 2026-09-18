# Bundle Service Architecture

This document describes the architecture of the Rossoctl bundle service and its HTTP API contract.

## Overview

The bundle service builds authorization bundles for AuthBridge clients and serves them as compressed tarballs.

Bundles are assembled from three policy tiers:

1. **Global policies** — cluster-wide rules defined in a global `AuthorizationPolicy` CR in the service namespace. The global CR owns the OPA query entry points (`package authbridge.inbound.request`, etc.) and contains the tier combination logic.
2. **Namespace policies** — scoped to the namespace from the client's SPIFFE ID.
3. **Client-specific policies** — scoped to the `AuthorizationPolicy` CR whose name and namespace match the SPIFFE ID.

The global CR serves as the decision combiner — it defines how the three tiers interact (e.g., whether namespace can override, whether all tiers must allow). Platform engineers can modify this logic by editing the global CR without code changes.

## Client identity

Clients identify themselves via a SPIFFE ID passed as a query parameter: `?spiffe={trust-domain}/ns/{namespace}/sa/{name}`. The service parses the namespace and name to locate the relevant policy resources.

Currently, no caller verification is performed (verification is handled upstream). In the future, the service will verify the caller's identity using either:

- **mTLS** — validate that the client certificate's SPIFFE SAN matches the claimed identity
- **JWT** — validate a Kubernetes service account token and confirm the token's namespace/name matches the claimed identity

The `Verifier` interface in `internal/bundleservice/identity/` is the extension point for this.

## Policy model

The service collects policies from `AuthorizationPolicy` CRs and packages them into the bundle payload.

- **Global policies** — `scope: global`, must reside in the service namespace (the Helm release namespace, `rossoctl-system` by default). These declare the OPA query entry-point packages (e.g., `package authbridge.inbound.request`) and contain the decision logic that combines namespace and client tiers.
- **Namespace policies** — `scope: namespace`, scoped to the namespace from the client's SPIFFE ID.
- **Client policies** — `scope: client`, scoped to the CR whose name and namespace match the SPIFFE ID.

### OPA query paths

The OPA plugin queries these four paths:

- `authbridge/inbound/request`
- `authbridge/inbound/response`
- `authbridge/outbound/request`
- `authbridge/outbound/response`

The global CR's Rego declares these packages directly. Namespace and client policies use sub-packages (e.g., `package authbridge.ns.inbound.request`, `package authbridge.client.inbound.request`).

### Default decision logic

The default global CR (`charts/operator/templates/bundleservice/default-policy.yaml`) implements:

```
allow if ns.override
allow if { ns_ok AND client_ok }
```

Where:
- `ns_ok` = namespace tier allows, or namespace tier is undefined (no namespace CR)
- `client_ok` = client tier allows, or client tier is undefined (no client CR)
- `ns.override` = namespace tier forces allow regardless of client tier

Platform engineers can modify this logic (e.g., remove namespace override support, add additional tiers, change from AND to OR) by editing the global CR.

### Bundle path structure

Inside the tar.gz bundle:

- `authbridge/global/<path>` — policies from the global CR
- `authbridge/ns/<path>` — policies from namespace CRs
- `authbridge/client/<path>` — policies from client CRs
- `.manifest` — OPA bundle manifest with revision hash and roots

OPA resolves policies by their `package` declaration, not their file path in the bundle.

## Caching

The bundle service uses three caches:

| Cache | Key | Value | Eviction |
|-------|-----|-------|----------|
| **ETagCache** | client ID | content hash | Invalidated on policy change |
| **BundleCache** | client ID | gzipped tar + hash | LRU + 1-minute TTL |
| **PolicyCache** | `"global"` or namespace | parsed policy entries + hash | Invalidated on CR change |

If the client sends an `If-None-Match` header matching the cached ETag, the service returns `304 Not Modified` without building a bundle.

### Cache invalidation

- Global CR change → invalidates all caches (all clients get fresh bundles)
- Namespace CR change → invalidates ETag and bundle caches for that namespace
- Client CR change → invalidates ETag and bundle caches for that specific client

## Concurrency control

The service uses two mechanisms to handle concurrent bundle requests efficiently:

1. **Singleflight** — deduplicates concurrent requests for the same client identity. If 10 pods with the same identity request simultaneously, only one build runs; the others wait for its result.
2. **Build semaphore** — limits concurrent bundle builds across different identities to 10. This prevents thundering herd scenarios (e.g., cluster restart causing 1000 clients to miss cache simultaneously) from overwhelming the Kubernetes API server and etcd.

Requests that cannot acquire a build slot block until one is available, respecting context cancellation (client disconnect).

## Bundle size limit

Bundles are limited to `5 MB`. If a generated bundle exceeds this size, the service returns `413 Payload Too Large`.

## HTTP API

### GET /bundles?spiffe={trust-domain}/ns/{namespace}/sa/{name}

Fetch the authorization bundle for the client identified by a SPIFFE ID.

Request:

- Method: `GET`
- Path: `/bundles`
- Query parameter: `spiffe={trust-domain}/ns/{namespace}/sa/{name}`
- Optional header: `If-None-Match: "<etag>"`

Response codes:

- `200 OK` — the bundle is returned in the response body
- `304 Not Modified` — the bundle has not changed since the provided ETag
- `400 Bad Request` — the SPIFFE ID could not be parsed or is missing
- `403 Forbidden` — identity verification failed
- `413 Payload Too Large` — bundle exceeds 5 MB
- `503 Service Unavailable` — service not ready
- `500 Internal Server Error` — internal failure

Response headers for `200 OK`:

- `Content-Type: application/gzip`
- `ETag: "<hash>"`
- `Cache-Control: max-age=0, must-revalidate`

Example request:

```bash
curl -v 'http://bundle-service.<namespace>.svc.cluster.local:8080/bundles?spiffe=localtest.me/ns/default/sa/my-agent'
```

ETag example:

```bash
curl -v -H 'If-None-Match: "sha256:abc123..."' \
  'http://bundle-service.<namespace>.svc.cluster.local:8080/bundles?spiffe=localtest.me/ns/default/sa/my-agent'
```

### GET /healthz

Returns `200 OK` and body `ok` when the service is alive.

### GET /readyz

Returns `200 OK` and body `ok` when the service is ready to serve bundles. If the service is not ready, it returns `503 Service Unavailable`.

## Startup and readiness

At startup, the service:

1. Loads Kubernetes configuration from in-cluster credentials or `KUBECONFIG`
2. Creates dynamic clients
3. Starts the `AuthorizationPolicy` informer with scope-based indexing
4. Waits for informer sync before serving HTTP traffic
5. Begins serving on port 8080

## Logging

The service logs structured messages via `log/slog`:

- Bundle requests received (URL, method)
- Bundle responses sent (URL, status, size)
- Bundle build events (namespace, name, hash)
- Policy change events (scope, namespace)
- Errors (conversion failures, oversized bundles)

Log level is configurable via `LOG_LEVEL` environment variable (`info`, `debug`, `warn`, `error`).

## Error handling

- Invalid or missing SPIFFE IDs return `400 Bad Request`
- Identity verification failures return `403 Forbidden`
- Oversized bundles return `413 Payload Too Large`
- Internal build or watch failures return `500 Internal Server Error`
- Readiness failures return `503 Service Unavailable`
