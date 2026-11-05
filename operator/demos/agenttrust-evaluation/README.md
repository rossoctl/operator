# AgentTrust Evaluation Demo

Run an [AgentTrust](https://github.com/leeyamin/agent-trust) evaluation as a Kubernetes Job. AgentTrust evaluates whether an AI agent's behavior aligns with its declared capabilities by probing it across in-scope, out-of-scope, and near-miss requests.

This demo is report-only — results are logged but no action is automatically enforced.

## Prerequisites

- A Kubernetes cluster with kubectl access
- A running A2A agent with an Agent Card endpoint
- An Anthropic API key for the LLM judge (AgentTrust uses the Claude Agent SDK, which also supports Vertex AI and Bedrock via Claude Code CLI configuration)

## Setup

### 1. Create the credentials secret

```bash
kubectl create secret generic agenttrust-judge-credentials \
  --from-literal=ANTHROPIC_API_KEY=<your-key>
```

See `secret.example.yaml` for the expected format.

### 2. Configure the evaluation

Edit `configmap.yaml` to set the target agent and evaluation parameters:

| Variable | Default | Description |
|----------|---------|-------------|
| `AGENT_URL` | `http://weather-agent.agents.svc:8000` | A2A endpoint of the target agent |
| `AGENTTRUST_NUM_PROBES` | `5` | Probes per scope |
| `AGENTTRUST_TRACE_SOURCE` | `none` | `mlflow` or `none` |
| `MLFLOW_EXPERIMENT_NAME` | `agenttrust-evaluations` | MLflow experiment name |
| `CLAUDE_CONFIG_DIR` | `/work` | Writable directory for CLI session data |

For the full list of configuration options, see the [AgentTrust CLI Reference](https://github.com/leeyamin/agent-trust#cli-reference).

### 3. Deploy

```bash
kubectl apply -k demos/agenttrust-evaluation/
```

### 4. View results

```bash
kubectl logs job/agenttrust-evaluation
```

The Job outputs a compact JSON result to stdout:

```json
{
  "schemaVersion": "v1",
  "evaluationId": "abc-123",
  "agent": "weather_agent",
  "cardHash": "sha256:...",
  "outcome": "completed",
  "alignmentPassed": true,
  "score": 85,
  "evidenceMode": "text",
  "completedAt": "2026-01-01T00:00:00+00:00",
  "reportURI": null
}
```

| Field | Description |
|-------|-------------|
| `outcome` | `completed` or `error` |
| `alignmentPassed` | `true` if score >= alignment threshold |
| `score` | Trust score (0-100) |
| `evidenceMode` | `text` or `text+trace` |
| `reportURI` | MLflow report URI when `MLFLOW_TRACKING_URI` is set |

## Cleanup and rerun

```bash
kubectl delete job agenttrust-evaluation
kubectl apply -k demos/agenttrust-evaluation/
```

K8s Jobs are immutable — delete before re-applying.

## Security

- Only evaluate sandboxed agents with read-only tools
- The container runs as non-root with a read-only filesystem
- No Kubernetes API permissions are granted to the pod
- No evaluation result is automatically enforced — results are informational only
