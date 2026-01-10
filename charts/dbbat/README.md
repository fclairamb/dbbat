# DBBat Helm Chart

PostgreSQL observability proxy for query tracking, access control, and safety.

## TL;DR

```bash
# Create namespace
kubectl create namespace dbbat

# Create secret for encryption key
kubectl create secret generic dbbat-secrets \
  --namespace dbbat \
  --from-literal=DBB_KEY=$(openssl rand -base64 32)

# Install chart
helm install dbbat oci://ghcr.io/fclairamb/charts/dbbat \
  --namespace dbbat \
  --set postgresql.external.dsn="postgres://user:pass@postgres.example.com:5432/dbbat?sslmode=require" \
  --set secrets.existingSecret=dbbat-secrets
```

## Introduction

This chart deploys DBBat, a transparent PostgreSQL proxy that provides:

- Query observability and logging
- User access control with time-windowed grants
- Connection and query tracking
- Encrypted credential storage
- REST API for management

## Prerequisites

- Kubernetes 1.19+
- Helm 3.8+
- External PostgreSQL database for DBBat storage

## Installing the Chart

### From OCI Registry

```bash
helm install dbbat oci://ghcr.io/fclairamb/charts/dbbat \
  --namespace dbbat \
  --create-namespace \
  --values values.yaml
```

### With Custom Values

Create a `values.yaml` file:

```yaml
postgresql:
  external:
    host: "postgres.internal.example.com"
    port: 5432
    database: "dbbat"
    username: "dbbat"
    sslMode: "require"

secrets:
  existingSecret: "dbbat-secrets"

ingress:
  enabled: true
  className: nginx
  hosts:
    - host: dbbat.example.com
      paths:
        - path: /
          pathType: Prefix
```

Install with:

```bash
helm install dbbat oci://ghcr.io/fclairamb/charts/dbbat \
  --namespace dbbat \
  --values values.yaml
```

## Uninstalling the Chart

```bash
helm uninstall dbbat --namespace dbbat
```

## Configuration

### Database Configuration

DBBat requires an external PostgreSQL database. You can configure it using either a full DSN or individual components.

#### Option 1: Full DSN

```yaml
postgresql:
  external:
    dsn: "postgres://user:pass@host:5432/dbbat?sslmode=require"
```

#### Option 2: Individual Components

```yaml
postgresql:
  external:
    host: "postgres.example.com"
    port: 5432
    database: "dbbat"
    username: "dbbat"
    password: "secretpass"  # Not recommended - use secrets instead
    sslMode: "require"
```

### Secret Management

#### Inline Secrets (Development)

```yaml
secrets:
  encryptionKey: "base64-encoded-key-here"
  databasePassword: "postgres-password"
```

Generate an encryption key:

```bash
openssl rand -base64 32
```

#### External Secrets (Production)

```yaml
secrets:
  existingSecret: "dbbat-secrets"
  keys:
    encryptionKey: "DBB_KEY"
    databasePassword: "DB_PASSWORD"
    dsn: "DBB_DSN"
```

Create the secret:

```bash
kubectl create secret generic dbbat-secrets \
  --from-literal=DBB_KEY=$(openssl rand -base64 32) \
  --from-literal=DB_PASSWORD=postgres-password \
  --from-literal=DBB_DSN="postgres://user:pass@host:5432/dbbat?sslmode=require"
```

### Service Configuration

#### API Service (HTTP)

```yaml
service:
  api:
    type: ClusterIP
    port: 8080
    annotations: {}
```

#### PostgreSQL Proxy Service (TCP)

```yaml
service:
  proxy:
    enabled: true
    type: ClusterIP  # or NodePort, LoadBalancer
    port: 5434
    annotations: {}
```

For AWS Network Load Balancer:

```yaml
service:
  proxy:
    type: LoadBalancer
    annotations:
      service.beta.kubernetes.io/aws-load-balancer-type: nlb
      service.beta.kubernetes.io/aws-load-balancer-scheme: internal
```

### Ingress Configuration

```yaml
ingress:
  enabled: true
  className: "nginx"
  annotations:
    cert-manager.io/cluster-issuer: letsencrypt-prod
  hosts:
    - host: dbbat.example.com
      paths:
        - path: /
          pathType: Prefix
  tls:
    - secretName: dbbat-tls
      hosts:
        - dbbat.example.com
```

### Security Configuration

#### Pod Security

The chart enforces security best practices by default:

```yaml
podSecurityContext:
  runAsNonRoot: true
  runAsUser: 65532
  runAsGroup: 65532
  fsGroup: 65532
  seccompProfile:
    type: RuntimeDefault

securityContext:
  allowPrivilegeEscalation: false
  readOnlyRootFilesystem: true
  capabilities:
    drop:
      - ALL
```

#### Network Policy

```yaml
networkPolicy:
  enabled: true
  allowedNamespaces:
    - ingress-nginx
  allowedPodLabels:
    app: my-app
```

#### Pod Disruption Budget

```yaml
podDisruptionBudget:
  enabled: true
  minAvailable: 2
```

### Resource Limits

```yaml
resources:
  limits:
    cpu: 500m
    memory: 256Mi
  requests:
    cpu: 100m
    memory: 128Mi
```

### High Availability

```yaml
replicaCount: 3

affinity:
  podAntiAffinity:
    requiredDuringSchedulingIgnoredDuringExecution:
      - labelSelector:
          matchLabels:
            app.kubernetes.io/name: dbbat
        topologyKey: topology.kubernetes.io/zone

podDisruptionBudget:
  enabled: true
  minAvailable: 2
```

## Usage Examples

### Minimal Installation

```bash
kubectl create namespace dbbat

kubectl create secret generic dbbat-secrets \
  --namespace dbbat \
  --from-literal=DBB_KEY=$(openssl rand -base64 32)

helm install dbbat oci://ghcr.io/fclairamb/charts/dbbat \
  --namespace dbbat \
  --set postgresql.external.dsn="postgres://user:pass@postgres.example.com:5432/dbbat?sslmode=require" \
  --set secrets.existingSecret=dbbat-secrets
```

### Production Installation

Create `values-production.yaml`:

```yaml
replicaCount: 3

postgresql:
  external:
    host: "postgres.internal.example.com"
    port: 5432
    database: "dbbat"
    username: "dbbat"
    sslMode: "verify-full"

secrets:
  existingSecret: "dbbat-secrets"

service:
  proxy:
    type: LoadBalancer
    annotations:
      service.beta.kubernetes.io/aws-load-balancer-type: nlb
      service.beta.kubernetes.io/aws-load-balancer-scheme: internal

ingress:
  enabled: true
  className: nginx
  annotations:
    cert-manager.io/cluster-issuer: letsencrypt-prod
  hosts:
    - host: dbbat.example.com
      paths:
        - path: /
          pathType: Prefix
  tls:
    - secretName: dbbat-tls
      hosts:
        - dbbat.example.com

podDisruptionBudget:
  enabled: true
  minAvailable: 2

affinity:
  podAntiAffinity:
    requiredDuringSchedulingIgnoredDuringExecution:
      - labelSelector:
          matchLabels:
            app.kubernetes.io/name: dbbat
        topologyKey: topology.kubernetes.io/zone
```

Install:

```bash
helm install dbbat oci://ghcr.io/fclairamb/charts/dbbat \
  --namespace dbbat \
  --values values-production.yaml
```

### Connecting to DBBat

#### Web UI

```bash
# Port-forward to access locally
kubectl port-forward --namespace dbbat svc/dbbat 8080:8080

# Open browser
open http://localhost:8080/app/
```

Default credentials: `admin` / `admin` (change immediately!)

#### PostgreSQL Proxy

```bash
# Port-forward the proxy service
kubectl port-forward --namespace dbbat svc/dbbat-pg 5434:5434

# Connect with psql
psql -h localhost -p 5434 -U <username> -d <database>
```

## Configuration Parameters

| Parameter | Description | Default |
|-----------|-------------|---------|
| `replicaCount` | Number of replicas | `1` |
| `image.repository` | Container image repository | `ghcr.io/fclairamb/dbbat` |
| `image.pullPolicy` | Image pull policy | `IfNotPresent` |
| `image.tag` | Image tag (defaults to Chart appVersion) | `""` |
| `postgresql.external.dsn` | Full PostgreSQL DSN | `""` |
| `postgresql.external.host` | PostgreSQL host | `""` |
| `postgresql.external.port` | PostgreSQL port | `5432` |
| `postgresql.external.database` | Database name | `"dbbat"` |
| `postgresql.external.username` | Database username | `"dbbat"` |
| `postgresql.external.sslMode` | SSL mode | `"require"` |
| `secrets.encryptionKey` | AES-256 encryption key (base64) | `""` |
| `secrets.existingSecret` | Use existing secret | `""` |
| `service.api.type` | API service type | `ClusterIP` |
| `service.api.port` | API service port | `8080` |
| `service.proxy.enabled` | Enable proxy service | `true` |
| `service.proxy.type` | Proxy service type | `ClusterIP` |
| `service.proxy.port` | Proxy service port | `5434` |
| `ingress.enabled` | Enable ingress | `false` |
| `ingress.className` | Ingress class name | `""` |
| `serviceAccount.create` | Create service account | `true` |
| `podSecurityContext.runAsUser` | User ID | `65532` |
| `resources.limits.cpu` | CPU limit | `500m` |
| `resources.limits.memory` | Memory limit | `256Mi` |
| `podDisruptionBudget.enabled` | Enable PDB | `false` |
| `networkPolicy.enabled` | Enable network policy | `false` |

For a complete list of parameters, see [values.yaml](values.yaml).

## Monitoring

### Prometheus Metrics

Add Prometheus scrape annotations:

```yaml
podAnnotations:
  prometheus.io/scrape: "true"
  prometheus.io/port: "8080"
  prometheus.io/path: "/api/v1/metrics"
```

## Upgrading

### To a New Chart Version

```bash
helm upgrade dbbat oci://ghcr.io/fclairamb/charts/dbbat \
  --namespace dbbat \
  --values values.yaml
```

### Rolling Back

```bash
helm rollback dbbat --namespace dbbat
```

## Troubleshooting

### Check Pod Status

```bash
kubectl get pods --namespace dbbat
kubectl describe pod <pod-name> --namespace dbbat
kubectl logs <pod-name> --namespace dbbat
```

### Check Service Status

```bash
kubectl get svc --namespace dbbat
```

### Test Database Connection

```bash
kubectl exec -it <pod-name> --namespace dbbat -- sh
# Inside the pod, check environment variables
env | grep DBB
```

## License

DBBat is licensed under the MIT License.

## Support

- GitHub: https://github.com/fclairamb/dbbat
- Issues: https://github.com/fclairamb/dbbat/issues
