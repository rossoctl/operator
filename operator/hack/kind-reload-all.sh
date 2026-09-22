#!/usr/bin/env bash
# Build the operator image, load it into a Kind cluster, and deploy.
# Creates deployments if they don't exist, updates them if they do.
#
# All three services ship in the SINGLE operator image and are selected by the
# container's `command:` (ENTRYPOINT is /manager):
#   1. rossoctl-controller-manager (/manager)
#   2. bundle-service              (/bundle-service)
#   3. token-broker                (/token-broker)
#
# token-broker additionally needs OAuth credentials; see the .env note below.
#
# Usage:
#   ./hack/kind-reload-all.sh [kind-cluster-name] [namespace]
#
# Defaults:
#   cluster:   rossoctl
#   namespace: rossoctl-system

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"

CLUSTER="${1:-rossoctl}"
NAMESPACE="${2:-rossoctl-system}"
CONTAINER_TOOL="${CONTAINER_TOOL:-docker}"
IMAGE_TAG="$(git -C "${ROOT_DIR}" rev-parse --short HEAD)"

# One image, three binaries — see the header.
OPERATOR_IMG="localhost/operator:${IMAGE_TAG}"

echo "============================================"
echo " Building and loading to Kind: ${CLUSTER}"
echo " Namespace: ${NAMESPACE}"
echo " Tag: ${IMAGE_TAG}"
echo "============================================"

# --- Ensure namespace ---

echo ""
echo "==> Ensuring namespace '${NAMESPACE}' exists"
kubectl create namespace "${NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -

# --- Ensure CRDs ---

echo ""
echo "==> Ensuring CRDs are installed"
if [ -f "${ROOT_DIR}/config/crd/bases/agent.rossoctl.dev_authorizationpolicies.yaml" ]; then
  kubectl apply -f "${ROOT_DIR}/config/crd/bases/agent.rossoctl.dev_authorizationpolicies.yaml"
fi

# --- Build images ---

echo ""
echo "==> Building rossoctl-operator image (manager + bundle-service + token-broker)"
${CONTAINER_TOOL} build -t "${OPERATOR_IMG}" -f "${ROOT_DIR}/Dockerfile" "${ROOT_DIR}"

# --- Load into Kind ---

echo ""
echo "==> Loading images into Kind cluster '${CLUSTER}'"
kind load docker-image "${OPERATOR_IMG}" --name "${CLUSTER}"

# --- Deploy rossoctl-controller-manager ---

echo ""
echo "==> Deploying rossoctl-controller-manager"
if kubectl get deployment rossoctl-controller-manager -n "${NAMESPACE}" &>/dev/null; then
  kubectl set image deployment/rossoctl-controller-manager \
    manager="${OPERATOR_IMG}" \
    -n "${NAMESPACE}"
else
  echo "    Deployment not found — install the operator with 'make deploy' first"
  echo "    (the controller-manager requires webhook certs, RBAC, and CRDs from kustomize)"
fi

# --- Deploy bundle-service (if source exists) ---

if [ -d "${ROOT_DIR}/cmd/bundle-service" ]; then
echo ""
echo "==> Deploying bundle-service"

# Rendered from the chart — the single source of truth — so this dev path uses the
# same manifests, RBAC and `command:` selector as a real install. The default policy
# must land in ${NAMESPACE}: the watcher ignores global-scope CRs from any other
# namespace, logging only a warning.
helm template rossoctl-operator "${ROOT_DIR}/../charts/operator" \
  --namespace "${NAMESPACE}" \
  --set bundleService.enabled=true \
  --show-only templates/bundleservice/serviceaccount.yaml \
  --show-only templates/bundleservice/rbac.yaml \
  --show-only templates/bundleservice/deployment.yaml \
  --show-only templates/bundleservice/service.yaml \
  --show-only templates/bundleservice/networkpolicy.yaml \
  --show-only templates/bundleservice/default-policy.yaml \
  | kubectl apply -f -

# Point at the freshly built, git-tagged local image; kind-loaded images need Never.
kubectl set image deployment/bundle-service \
  bundle-service="${OPERATOR_IMG}" \
  -n "${NAMESPACE}"
kubectl patch deployment/bundle-service -n "${NAMESPACE}" --type=json \
  -p='[{"op":"replace","path":"/spec/template/spec/containers/0/imagePullPolicy","value":"Never"}]'
echo "    bundle-service deployed from chart (image ${OPERATOR_IMG})"
fi

# --- Deploy token-broker (if source exists) ---
#
# Skipped unless operator/.env supplies OAuth credentials; the broker refuses to
# start without a client ID and secret.

if [ -d "${ROOT_DIR}/cmd/token-broker" ]; then
echo ""
echo "==> Deploying token-broker"

# Load OAuth credentials from .env.
# Sourced rather than piped through xargs: xargs word-splits on whitespace, so a
# value containing a space would export a truncated variable, pass the guard
# below, and create the Secret with a partial credential — failing later at OAuth
# time with an error that points at the provider instead of at this loader.
if [ -f "${ROOT_DIR}/.env" ]; then
  set -a
  # shellcheck disable=SC1091
  . "${ROOT_DIR}/.env"
  set +a
fi

if [ -z "${GITHUB_OAUTH_CLIENT_ID:-}" ] || [ -z "${GITHUB_OAUTH_CLIENT_SECRET:-}" ]; then
  echo "    SKIPPED: OAuth credentials not set."
  echo "    To deploy token-broker, create ${ROOT_DIR}/.env with:"
  echo "      GITHUB_OAUTH_CLIENT_ID=<your-client-id>"
  echo "      GITHUB_OAUTH_CLIENT_SECRET=<your-client-secret>"
else

# Create/update OAuth secret
kubectl delete secret github-oauth-credentials -n "${NAMESPACE}" 2>/dev/null || true
kubectl create secret generic github-oauth-credentials \
  --from-literal=client-id="${GITHUB_OAUTH_CLIENT_ID}" \
  --from-literal=client-secret="${GITHUB_OAUTH_CLIENT_SECRET}" \
  --namespace="${NAMESPACE}"
echo "    OAuth secret created"

# Render the token-broker manifests from the chart — the single source of truth —
# rather than duplicating them here. The secret is created above from .env.
helm template rossoctl-operator "${ROOT_DIR}/../charts/operator" \
  --namespace "${NAMESPACE}" \
  --set tokenBroker.enabled=true \
  --show-only templates/tokenbroker/serviceaccount.yaml \
  --show-only templates/tokenbroker/deployment.yaml \
  --show-only templates/tokenbroker/service.yaml \
  --show-only templates/tokenbroker/httproute.yaml \
  | kubectl apply -f -

# Point at the freshly built, git-tagged local image; kind-loaded images need Never.
kubectl set image deployment/token-broker \
  token-broker="${OPERATOR_IMG}" \
  -n "${NAMESPACE}"
kubectl patch deployment/token-broker -n "${NAMESPACE}" --type=json \
  -p='[{"op":"replace","path":"/spec/template/spec/containers/0/imagePullPolicy","value":"Never"}]'
echo "    token-broker deployed from chart (image ${OPERATOR_IMG})"
fi
fi

# --- Delete pods to pick up the new images ---

echo ""
echo "==> Deleting pods to pick up new images"
kubectl delete pods -n "${NAMESPACE}" -l control-plane=controller-manager --wait=false 2>/dev/null || true

if [ -d "${ROOT_DIR}/cmd/bundle-service" ]; then
  kubectl delete pods -n "${NAMESPACE}" -l app=bundle-service --wait=false
fi

if [ -d "${ROOT_DIR}/cmd/token-broker" ]; then
  kubectl delete pods -n "${NAMESPACE}" -l app=token-broker --wait=false
fi

# --- Wait for rollouts ---

echo ""
echo "==> Waiting for rossoctl-controller-manager rollout"
kubectl rollout status deployment/rossoctl-controller-manager -n "${NAMESPACE}" --timeout=120s 2>/dev/null || \
  echo "    WARNING: rossoctl-controller-manager rollout did not complete"

if [ -d "${ROOT_DIR}/cmd/bundle-service" ]; then
  echo "==> Waiting for bundle-service rollout"
  kubectl rollout status deployment/bundle-service -n "${NAMESPACE}" --timeout=60s
fi

if [ -d "${ROOT_DIR}/cmd/token-broker" ]; then
  echo "==> Waiting for token-broker rollout"
  kubectl rollout status deployment/token-broker -n "${NAMESPACE}" --timeout=60s
fi

# --- Summary ---

echo ""
echo "============================================"
echo " Done!"
echo ""
echo " Images loaded:"
echo "   ${OPERATOR_IMG}  (manager + bundle-service + token-broker)"
echo ""
echo " Namespace: ${NAMESPACE}"
echo "   - rossoctl-controller-manager"
if [ -d "${ROOT_DIR}/cmd/bundle-service" ]; then
  echo "   - bundle-service"
fi
if [ -d "${ROOT_DIR}/cmd/token-broker" ]; then
  echo "   - token-broker"
fi
echo ""
if [ -d "${ROOT_DIR}/cmd/bundle-service" ]; then
  echo " bundle-service URL: http://bundle-service.${NAMESPACE}.svc.cluster.local:8080"
fi
if [ -d "${ROOT_DIR}/cmd/token-broker" ]; then
  echo " token-broker URL:   http://token-broker.${NAMESPACE}.svc.cluster.local:8190"
  echo " OAuth callback:     http://token-broker.localtest.me:8080/oauth/callback (via 'http' gateway)"
fi
echo ""
echo " To port-forward:"
if [ -d "${ROOT_DIR}/cmd/bundle-service" ]; then
  echo "   kubectl port-forward -n ${NAMESPACE} svc/bundle-service 8080:8080"
fi
if [ -d "${ROOT_DIR}/cmd/token-broker" ]; then
  echo "   kubectl port-forward -n ${NAMESPACE} svc/token-broker 8190:8190"
fi
echo "============================================"
