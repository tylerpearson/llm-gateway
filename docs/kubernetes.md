# Kubernetes deployment

The gateway ships with a Helm chart in [`charts/llm-gateway`](../charts/llm-gateway).
The chart deploys the gateway only. MySQL, ClickHouse, and Redis are treated as
external dependencies you point at with connection strings, so you can use
managed services or your own operators for the stateful tier.

Chart resources: Deployment, Service, Ingress (optional), HorizontalPodAutoscaler
(optional), ConfigMap (the gateway config), Secret (API keys and DSNs, optional),
and ServiceAccount.

Prerequisites:

- A Kubernetes cluster and `kubectl` context.
- Helm 3.
- Reachable MySQL, ClickHouse, and Redis instances (or leave their DSNs empty to
  run without auth, analytics, or caching, which is not recommended in a
  cluster).
- A container image. The chart defaults to `ghcr.io/tylerpearson/llm-gateway`
  at the chart `appVersion`; override `image.repository` and `image.tag` to use
  your own build.

## Validate the chart

Always render and lint before installing:

```bash
helm lint charts/llm-gateway
helm template llm-gateway charts/llm-gateway | less
```

## Configure secrets

Provider keys and datastore DSNs are injected as environment variables from a
Kubernetes Secret. The config file references them by name (`api_key_env`,
`mysql_dsn_env`, and so on), so the values themselves never appear in the
ConfigMap.

You have two options.

**Option 1: let the chart manage the Secret.** Supply values at install time
(never commit real keys). Use `--set` from a secret manager, or a values file
kept out of version control:

```bash
helm upgrade --install llm-gateway charts/llm-gateway \
  --namespace llm-gateway --create-namespace \
  --set secretEnv.values.ANTHROPIC_API_KEY="$ANTHROPIC_API_KEY" \
  --set secretEnv.values.OPENAI_API_KEY="$OPENAI_API_KEY" \
  --set secretEnv.values.MYSQL_DSN="$MYSQL_DSN" \
  --set secretEnv.values.CLICKHOUSE_DSN="$CLICKHOUSE_DSN" \
  --set secretEnv.values.REDIS_ADDR="$REDIS_ADDR"
```

**Option 2: bring your own Secret** (recommended for production, works with
sealed-secrets or external-secrets). Set `secretEnv.create: false` and name an
existing Secret whose keys match `secretEnv.envKeys`:

```yaml
secretEnv:
  create: false
  existingSecret: llm-gateway-secrets
  envKeys:
    - ANTHROPIC_API_KEY
    - OPENAI_API_KEY
    - GLM_API_KEY
    - MYSQL_DSN
    - CLICKHOUSE_DSN
    - REDIS_ADDR
```

## Configure the gateway

The `config` block in `values.yaml` mirrors `configs/config.example.yaml` and is
rendered into a ConfigMap mounted at `/app/configs/config.yaml`. Set providers,
model aliases, limits, and the storage env-var names there. A minimal
production-shaped override:

```yaml
config:
  logging:
    level: info
    format: json
  routing:
    default_alias: default
    aliases:
      default:
        provider: anthropic
        model: claude-haiku-4-5-20251001
      frontier:
        provider: anthropic
        model: claude-opus-4-8
  limits:
    mode: soft
    per_team:
      monthly_usd: 500
  attribution:
    tag_headers:
      - x-team-project
```

Storage DSNs are supplied through the Secret, not this block; the `storage`
section only names the environment variables to read.

## Apply database migrations

The gateway does not migrate on startup, and the chart does not include a
migration Job. Apply the schema to your external MySQL and ClickHouse once
before (or immediately after) the first install, using `gatewayctl` with the
same DSNs the gateway will use. For example, as a one-off Job or from a machine
with network access to the datastores:

```bash
MYSQL_DSN="$MYSQL_DSN" CLICKHOUSE_DSN="$CLICKHOUSE_DSN" gatewayctl migrate
```

Then seed at least one team and virtual key so clients can authenticate:

```bash
MYSQL_DSN="$MYSQL_DSN" gatewayctl team create acme
MYSQL_DSN="$MYSQL_DSN" gatewayctl key create --team <team-id> --name prod
```

## Expose the gateway

By default the Service is `ClusterIP` on port 8080. For quick access without an
ingress, port-forward:

```bash
kubectl -n llm-gateway port-forward svc/llm-gateway 8080:8080
```

To expose it externally, enable the Ingress and set your class, host, and TLS:

```yaml
ingress:
  enabled: true
  className: nginx
  annotations:
    cert-manager.io/cluster-issuer: letsencrypt-prod
  hosts:
    - host: llm-gateway.example.com
      paths:
        - path: /
          pathType: Prefix
  tls:
    - secretName: llm-gateway-tls
      hosts:
        - llm-gateway.example.com
```

## Autoscaling

The gateway is stateless (all state lives in the datastores), so it scales
horizontally. Enable the HorizontalPodAutoscaler:

```yaml
autoscaling:
  enabled: true
  minReplicas: 2
  maxReplicas: 10
  targetCPUUtilizationPercentage: 70
```

When `autoscaling.enabled` is true, `replicaCount` is ignored.

## Metrics and dashboards

Pods carry Prometheus scrape annotations (`prometheus.io/scrape: "true"`,
port 8080, path `/metrics`) so a Prometheus configured for pod-annotation
discovery picks them up automatically. Adjust or disable them under
`podAnnotations`. The Grafana dashboards under
[`deploy/grafana/provisioning/dashboards`](../deploy/grafana/provisioning/dashboards)
can be imported into your Grafana; they read from ClickHouse (spend, FinOps,
cache) and Prometheus (latency, reliability).

## Security posture

The chart runs the container as a non-root user (uid 65532) with a read-only
root filesystem, no privilege escalation, and all Linux capabilities dropped.
Keep these defaults unless you have a specific reason to relax them.

## Upgrades and key rotation

Roll out a new image or config with the same command used to install:

```bash
helm upgrade llm-gateway charts/llm-gateway --namespace llm-gateway --reuse-values \
  --set image.tag=<new-tag>
```

To rotate provider keys without redeploying, edit the Secret and restart the
Deployment so pods pick up the new values:

```bash
kubectl -n llm-gateway edit secret <gateway-secret-name>
kubectl -n llm-gateway rollout restart deploy/llm-gateway
```

`helm install` prints these same operational hints (URL, health checks, secret
edit, rollout restart) from the chart's `NOTES.txt`.
