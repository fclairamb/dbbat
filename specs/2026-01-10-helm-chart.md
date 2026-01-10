# Helm Chart for DBBat

## Status: Draft

## Summary

Create a Helm chart for deploying DBBat to Kubernetes clusters. The chart will be published to GitHub Container Registry (ghcr.io) alongside container images, enabling one-command deployment with `helm install`.

## Goals

1. **Simple deployment**: `helm install dbbat oci://ghcr.io/fclairamb/charts/dbbat`
2. **Production-ready defaults**: Security-hardened, resource-constrained, proper health checks
3. **Flexible configuration**: Support common deployment patterns without overwhelming complexity
4. **GitOps-friendly**: Declarative values, immutable defaults, predictable upgrades

## Container Image Source

The Helm chart will use container images published to GHCR by the release workflow:

```
ghcr.io/fclairamb/dbbat:latest
ghcr.io/fclairamb/dbbat:1.0.0
```

## Chart Distribution

### Option A: OCI Registry in GHCR (Recommended)

Store the Helm chart as an OCI artifact in GHCR alongside container images:

```bash
# Install from OCI registry
helm install dbbat oci://ghcr.io/fclairamb/charts/dbbat --version 1.0.0
```

**Pros:**
- Single registry for both images and charts
- No additional infrastructure needed
- Native Helm 3.8+ support
- Versioned alongside the application

**Cons:**
- Slightly newer Helm feature (3.8+, released 2022)
- No web-based chart browsing

### Option B: GitHub Pages Helm Repository

Host a traditional Helm repository on GitHub Pages:

```bash
helm repo add dbbat https://fclairamb.github.io/dbbat/charts
helm install dbbat dbbat/dbbat
```

**Pros:**
- Works with all Helm versions
- Browseable index.yaml
- Familiar pattern

**Cons:**
- Extra build step for index generation
- Separate hosting concern

### Recommendation

**Use Option A (OCI Registry)** for simplicity. Helm 3.8+ is widely adopted, and keeping charts in GHCR alongside images reduces complexity. Add Option B later if there's demand for legacy Helm support.

## Chart Structure

```
charts/
└── dbbat/
    ├── Chart.yaml
    ├── values.yaml
    ├── templates/
    │   ├── _helpers.tpl
    │   ├── deployment.yaml
    │   ├── service.yaml
    │   ├── service-pg.yaml          # Separate service for PostgreSQL proxy
    │   ├── configmap.yaml
    │   ├── secret.yaml
    │   ├── ingress.yaml
    │   ├── serviceaccount.yaml
    │   ├── hpa.yaml                  # HorizontalPodAutoscaler (optional)
    │   ├── pdb.yaml                  # PodDisruptionBudget (optional)
    │   ├── networkpolicy.yaml        # Network policies (optional)
    │   └── NOTES.txt
    ├── values.schema.json            # JSON Schema for values validation
    └── README.md
```

## Architecture Decisions

### Decision 1: PostgreSQL Deployment Strategy

DBBat requires PostgreSQL for its internal storage. Three options:

#### Option A: External PostgreSQL Only (Recommended)

Require users to provide an external PostgreSQL DSN. No bundled database.

```yaml
# values.yaml
postgresql:
  external:
    dsn: "postgres://user:pass@hostname:5432/dbbat?sslmode=require"
    # OR components:
    host: "postgres.example.com"
    port: 5432
    database: "dbbat"
    username: "dbbat"
    password: ""           # From secret
    sslMode: "require"
```

**Pros:**
- Simplest chart
- Users manage their own database (RDS, Cloud SQL, managed PostgreSQL)
- No stateful complexity in the chart
- Follows "cattle not pets" philosophy

**Cons:**
- Requires external database before installing
- More initial setup for testing/development

#### Option B: Bundled PostgreSQL via Subchart

Include Bitnami's PostgreSQL chart as a dependency:

```yaml
# Chart.yaml
dependencies:
  - name: postgresql
    version: "~15.0"
    repository: "oci://registry-1.docker.io/bitnamicharts"
    condition: postgresql.enabled
```

**Pros:**
- One-command deployment for testing
- Good for dev/staging environments

**Cons:**
- Adds complexity
- Bundled PostgreSQL rarely appropriate for production
- Version pinning and security updates for dependency

#### Option C: CloudNativePG Operator Integration

Support CloudNativePG for Kubernetes-native PostgreSQL:

```yaml
postgresql:
  cloudnativepg:
    enabled: true
    instances: 3
    storage: 10Gi
```

**Pros:**
- Production-grade PostgreSQL on Kubernetes
- Automated backups, failover, etc.

**Cons:**
- Requires operator pre-installed
- Adds complexity

#### Recommendation

**Start with Option A** (external only) for the initial release. Document PostgreSQL setup clearly. Consider adding Option B as an optional subchart for development/testing use cases in a future version.

### Decision 2: Secret Management

Three approaches for handling sensitive values:

#### Option A: Inline Secrets (Simple)

Let users specify secrets directly in values:

```yaml
secrets:
  encryptionKey: "base64-encoded-key"
  databasePassword: "mypassword"
```

**Pros:**
- Simple for getting started
- Works everywhere

**Cons:**
- Secrets visible in Helm release history
- Not suitable for GitOps

#### Option B: External Secrets Reference

Reference existing Kubernetes secrets:

```yaml
secrets:
  existingSecret: "dbbat-secrets"
  encryptionKeyKey: "encryption-key"
  databasePasswordKey: "db-password"
```

**Pros:**
- Secrets managed externally (Vault, External Secrets Operator, sealed-secrets)
- GitOps-friendly

**Cons:**
- More setup required

#### Option C: Both (Recommended)

Support both patterns with external secrets taking precedence:

```yaml
secrets:
  # Option 1: Create secret from values
  encryptionKey: ""      # Base64-encoded AES-256 key
  databasePassword: ""

  # Option 2: Use existing secret (takes precedence)
  existingSecret: ""
  keys:
    encryptionKey: "DBB_KEY"
    databasePassword: "DB_PASSWORD"
```

### Decision 3: Ingress for PostgreSQL Proxy

The PostgreSQL proxy (port 5432) uses TCP, not HTTP. Standard Ingress doesn't support TCP well.

#### Options for PostgreSQL Access

| Approach | Description | When to Use |
|----------|-------------|-------------|
| **LoadBalancer Service** | Cloud-native L4 load balancer | Production on cloud providers |
| **NodePort** | Expose on node's port | Development, bare metal |
| **ClusterIP only** | Internal access only | Apps in same cluster connect |
| **TCP Ingress** | nginx-ingress ConfigMap for TCP | When nginx-ingress is available |
| **Gateway API TCPRoute** | Modern Kubernetes Gateway API | Future-proof, newer clusters |

#### Recommendation

Default to **ClusterIP** for security. Provide clear documentation for each exposure method. Include a separate `service-pg.yaml` that can be toggled to LoadBalancer/NodePort.

```yaml
# values.yaml
service:
  api:
    type: ClusterIP
    port: 8080
  proxy:
    enabled: true
    type: ClusterIP        # ClusterIP | NodePort | LoadBalancer
    port: 5432
    nodePort: ""           # Optional: fixed NodePort
    annotations: {}        # Cloud provider LB annotations
```

### Decision 4: Replica Count and Session Handling

DBBat's PostgreSQL proxy maintains TCP connections. Considerations for multiple replicas:

#### Connection Routing

- **PostgreSQL connections are long-lived**: Client connects once, issues many queries
- **No session affinity needed**: Each connection is independent
- **Load balancing works**: New connections distributed across replicas
- **In-flight connections survive pod restarts**: Clients reconnect automatically

#### Recommendation

- Default to 1 replica for simplicity
- Support multiple replicas for high availability
- No special session affinity needed (PostgreSQL clients handle reconnection)
- Add PodDisruptionBudget for safe rollouts

```yaml
replicaCount: 1

autoscaling:
  enabled: false
  minReplicas: 2
  maxReplicas: 5
  targetCPUUtilizationPercentage: 70

podDisruptionBudget:
  enabled: false
  minAvailable: 1
  # OR maxUnavailable: 1
```

## Values Schema

### Complete `values.yaml`

```yaml
# =============================================================================
# DBBat Helm Chart Values
# =============================================================================

# -- Number of replicas
replicaCount: 1

image:
  # -- Container image repository
  repository: ghcr.io/fclairamb/dbbat
  # -- Container image pull policy
  pullPolicy: IfNotPresent
  # -- Container image tag (defaults to Chart appVersion)
  tag: ""

# -- Image pull secrets for private registries
imagePullSecrets: []

# -- Override the chart name
nameOverride: ""
# -- Override the full resource name
fullnameOverride: ""

# =============================================================================
# Database Configuration
# =============================================================================
postgresql:
  # External PostgreSQL connection settings
  external:
    # -- Full DSN (takes precedence over individual settings)
    # Example: postgres://user:pass@host:5432/dbbat?sslmode=require
    dsn: ""

    # -- PostgreSQL host (used if dsn is empty)
    host: ""
    # -- PostgreSQL port
    port: 5432
    # -- Database name
    database: "dbbat"
    # -- Database username
    username: "dbbat"
    # -- Database password (use secrets.databasePassword for production)
    password: ""
    # -- SSL mode: disable, require, verify-ca, verify-full
    sslMode: "require"

# =============================================================================
# Secrets
# =============================================================================
secrets:
  # -- AES-256 encryption key (base64-encoded, 32 bytes raw)
  # Generate with: openssl rand -base64 32
  encryptionKey: ""

  # -- Database password (if not in DSN or external.password)
  databasePassword: ""

  # -- Use existing secret instead of creating one
  # The secret must contain keys specified in secrets.keys
  existingSecret: ""

  # -- Key names in existingSecret
  keys:
    encryptionKey: "DBB_KEY"
    databasePassword: "DB_PASSWORD"
    dsn: "DBB_DSN"

# =============================================================================
# Application Configuration
# =============================================================================
config:
  # -- PostgreSQL proxy listen address
  listenPg: ":5432"
  # -- REST API listen address
  listenApi: ":8080"
  # -- Base URL for frontend (default: /app)
  baseUrl: "/app"
  # -- Run mode: "" (default) or "test" (for testing only!)
  runMode: ""
  # -- Path to encryption key file (alternative to secrets.encryptionKey)
  # Mount a secret as a volume and reference the path here
  keyFile: ""

# =============================================================================
# Services
# =============================================================================
service:
  api:
    # -- API service type
    type: ClusterIP
    # -- API service port
    port: 8080
    # -- API service annotations
    annotations: {}

  proxy:
    # -- Enable PostgreSQL proxy service
    enabled: true
    # -- Proxy service type (ClusterIP, NodePort, LoadBalancer)
    type: ClusterIP
    # -- Proxy service port
    port: 5432
    # -- Fixed NodePort (optional, only for type: NodePort)
    nodePort: ""
    # -- Proxy service annotations (e.g., cloud LB configuration)
    annotations: {}
    # Example for AWS NLB:
    #   service.beta.kubernetes.io/aws-load-balancer-type: nlb
    #   service.beta.kubernetes.io/aws-load-balancer-scheme: internal

# =============================================================================
# Ingress (HTTP only - for API and Web UI)
# =============================================================================
ingress:
  # -- Enable ingress for API/Web UI
  enabled: false
  # -- Ingress class name
  className: ""
  # -- Ingress annotations
  annotations: {}
    # kubernetes.io/ingress.class: nginx
    # cert-manager.io/cluster-issuer: letsencrypt
  # -- Ingress hosts
  hosts:
    - host: dbbat.example.com
      paths:
        - path: /
          pathType: Prefix
  # -- Ingress TLS configuration
  tls: []
  #  - secretName: dbbat-tls
  #    hosts:
  #      - dbbat.example.com

# =============================================================================
# Service Account
# =============================================================================
serviceAccount:
  # -- Create a service account
  create: true
  # -- Service account annotations
  annotations: {}
  # -- Service account name (generated if not set)
  name: ""
  # -- Automount service account token
  automountServiceAccountToken: false

# =============================================================================
# Pod Configuration
# =============================================================================
podAnnotations: {}
  # prometheus.io/scrape: "true"
  # prometheus.io/port: "8080"
  # prometheus.io/path: "/api/v1/metrics"

podLabels: {}

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

# =============================================================================
# Resources
# =============================================================================
resources:
  limits:
    cpu: 500m
    memory: 256Mi
  requests:
    cpu: 100m
    memory: 128Mi

# =============================================================================
# Probes
# =============================================================================
livenessProbe:
  httpGet:
    path: /api/v1/health
    port: http
  initialDelaySeconds: 5
  periodSeconds: 10
  timeoutSeconds: 5
  failureThreshold: 3

readinessProbe:
  httpGet:
    path: /api/v1/health
    port: http
  initialDelaySeconds: 5
  periodSeconds: 5
  timeoutSeconds: 3
  failureThreshold: 3

# =============================================================================
# Autoscaling
# =============================================================================
autoscaling:
  enabled: false
  minReplicas: 2
  maxReplicas: 10
  targetCPUUtilizationPercentage: 70
  targetMemoryUtilizationPercentage: 80

# =============================================================================
# Pod Disruption Budget
# =============================================================================
podDisruptionBudget:
  enabled: false
  # -- Minimum available pods during disruption
  minAvailable: 1
  # -- Maximum unavailable pods (alternative to minAvailable)
  # maxUnavailable: 1

# =============================================================================
# Node Scheduling
# =============================================================================
nodeSelector: {}

tolerations: []

affinity: {}
  # podAntiAffinity:
  #   preferredDuringSchedulingIgnoredDuringExecution:
  #     - weight: 100
  #       podAffinityTerm:
  #         labelSelector:
  #           matchLabels:
  #             app.kubernetes.io/name: dbbat
  #         topologyKey: kubernetes.io/hostname

# =============================================================================
# Network Policy
# =============================================================================
networkPolicy:
  enabled: false
  # -- Allow ingress from these namespaces
  allowedNamespaces: []
  # -- Allow ingress from pods with these labels
  allowedPodLabels: {}

# =============================================================================
# Extra Configuration
# =============================================================================
# -- Additional environment variables
extraEnv: []
  # - name: EXTRA_VAR
  #   value: "value"

# -- Additional volume mounts
extraVolumeMounts: []

# -- Additional volumes
extraVolumes: []
```

## Key Templates

### `deployment.yaml` (excerpt)

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ include "dbbat.fullname" . }}
  labels:
    {{- include "dbbat.labels" . | nindent 4 }}
spec:
  replicas: {{ .Values.replicaCount }}
  selector:
    matchLabels:
      {{- include "dbbat.selectorLabels" . | nindent 6 }}
  template:
    metadata:
      annotations:
        checksum/config: {{ include (print $.Template.BasePath "/configmap.yaml") . | sha256sum }}
        checksum/secret: {{ include (print $.Template.BasePath "/secret.yaml") . | sha256sum }}
        {{- with .Values.podAnnotations }}
        {{- toYaml . | nindent 8 }}
        {{- end }}
      labels:
        {{- include "dbbat.labels" . | nindent 8 }}
        {{- with .Values.podLabels }}
        {{- toYaml . | nindent 8 }}
        {{- end }}
    spec:
      serviceAccountName: {{ include "dbbat.serviceAccountName" . }}
      securityContext:
        {{- toYaml .Values.podSecurityContext | nindent 8 }}
      containers:
        - name: {{ .Chart.Name }}
          image: "{{ .Values.image.repository }}:{{ .Values.image.tag | default .Chart.AppVersion }}"
          imagePullPolicy: {{ .Values.image.pullPolicy }}
          securityContext:
            {{- toYaml .Values.securityContext | nindent 12 }}
          ports:
            - name: http
              containerPort: 8080
              protocol: TCP
            - name: postgres
              containerPort: 5432
              protocol: TCP
          envFrom:
            - configMapRef:
                name: {{ include "dbbat.fullname" . }}
            - secretRef:
                name: {{ .Values.secrets.existingSecret | default (include "dbbat.fullname" .) }}
          resources:
            {{- toYaml .Values.resources | nindent 12 }}
          livenessProbe:
            {{- toYaml .Values.livenessProbe | nindent 12 }}
          readinessProbe:
            {{- toYaml .Values.readinessProbe | nindent 12 }}
```

### `service-pg.yaml`

```yaml
{{- if .Values.service.proxy.enabled }}
apiVersion: v1
kind: Service
metadata:
  name: {{ include "dbbat.fullname" . }}-pg
  labels:
    {{- include "dbbat.labels" . | nindent 4 }}
    app.kubernetes.io/component: proxy
  {{- with .Values.service.proxy.annotations }}
  annotations:
    {{- toYaml . | nindent 4 }}
  {{- end }}
spec:
  type: {{ .Values.service.proxy.type }}
  ports:
    - port: {{ .Values.service.proxy.port }}
      targetPort: postgres
      protocol: TCP
      name: postgres
      {{- if and (eq .Values.service.proxy.type "NodePort") .Values.service.proxy.nodePort }}
      nodePort: {{ .Values.service.proxy.nodePort }}
      {{- end }}
  selector:
    {{- include "dbbat.selectorLabels" . | nindent 4 }}
{{- end }}
```

## Chart Publishing Workflow

Add to `.github/workflows/release.yml`:

```yaml
  publish-chart:
    runs-on: ubuntu-latest
    needs: [release]  # After container images are pushed
    steps:
      - uses: actions/checkout@v6.0.1

      - name: Install Helm
        uses: azure/setup-helm@v4
        with:
          version: v3.17.0

      - name: Login to GHCR
        run: |
          echo "${{ secrets.GITHUB_TOKEN }}" | helm registry login ghcr.io -u ${{ github.actor }} --password-stdin

      - name: Package chart
        run: |
          # Update appVersion to match release
          VERSION=${GITHUB_REF_NAME#v}
          sed -i "s/^appVersion:.*/appVersion: \"$VERSION\"/" charts/dbbat/Chart.yaml
          sed -i "s/^version:.*/version: $VERSION/" charts/dbbat/Chart.yaml
          helm package charts/dbbat

      - name: Push chart to GHCR
        run: |
          helm push dbbat-*.tgz oci://ghcr.io/fclairamb/charts
```

## Usage Examples

### Minimal Installation

```bash
# Create namespace
kubectl create namespace dbbat

# Create secret for encryption key
kubectl create secret generic dbbat-secrets \
  --namespace dbbat \
  --from-literal=DBB_KEY=$(openssl rand -base64 32)

# Install with external PostgreSQL
helm install dbbat oci://ghcr.io/fclairamb/charts/dbbat \
  --namespace dbbat \
  --set postgresql.external.dsn="postgres://user:pass@postgres.example.com:5432/dbbat?sslmode=require" \
  --set secrets.existingSecret=dbbat-secrets
```

### Production Installation

```yaml
# values-production.yaml
replicaCount: 3

postgresql:
  external:
    host: "postgres.internal.example.com"
    port: 5432
    database: "dbbat"
    username: "dbbat"
    sslMode: "verify-full"

secrets:
  existingSecret: "dbbat-secrets"  # Managed by external-secrets-operator

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

autoscaling:
  enabled: true
  minReplicas: 3
  maxReplicas: 10

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

```bash
helm install dbbat oci://ghcr.io/fclairamb/charts/dbbat \
  --namespace dbbat \
  --values values-production.yaml
```

## Security Considerations

### 1. Pod Security

The chart enforces security best practices by default:

- **Non-root execution**: Container runs as user 65532 (distroless nonroot)
- **Read-only filesystem**: Root filesystem is read-only
- **Dropped capabilities**: All Linux capabilities dropped
- **Seccomp profile**: RuntimeDefault seccomp profile enabled
- **No privilege escalation**: Blocked by default

### 2. Secret Management

Recommendations for production:

1. **Never store secrets in values.yaml**: Use `existingSecret` reference
2. **Use External Secrets Operator**: Sync secrets from Vault, AWS Secrets Manager, etc.
3. **Rotate encryption keys**: The encryption key can be rotated (requires re-encryption of stored credentials)

### 3. Network Security

Consider enabling NetworkPolicy for production:

```yaml
networkPolicy:
  enabled: true
  allowedNamespaces:
    - ingress-nginx  # Allow ingress controller
  allowedPodLabels:
    app: my-app      # Allow specific apps to connect to proxy
```

### 4. TLS for PostgreSQL Proxy

The PostgreSQL proxy accepts unencrypted connections by default. For encryption:

1. **Terminate TLS at load balancer**: Use AWS NLB with TLS listener
2. **Use a service mesh**: Istio/Linkerd can add mTLS
3. **Future: Native TLS**: Could be added to DBBat itself

## Monitoring and Observability

### Prometheus Metrics

Add Prometheus scrape annotations:

```yaml
podAnnotations:
  prometheus.io/scrape: "true"
  prometheus.io/port: "8080"
  prometheus.io/path: "/api/v1/metrics"
```

### Recommended Alerts

```yaml
# PrometheusRule example (if using prometheus-operator)
groups:
  - name: dbbat
    rules:
      - alert: DBBatDown
        expr: up{job="dbbat"} == 0
        for: 1m
        labels:
          severity: critical
        annotations:
          summary: "DBBat is down"

      - alert: DBBatHighErrorRate
        expr: rate(dbbat_queries_failed_total[5m]) > 0.1
        for: 5m
        labels:
          severity: warning
```

## Future Enhancements

### Phase 2: Optional Bundled PostgreSQL

Add Bitnami PostgreSQL as optional dependency:

```yaml
# Chart.yaml
dependencies:
  - name: postgresql
    version: "~16.0"
    repository: "oci://registry-1.docker.io/bitnamicharts"
    condition: postgresql.bundled.enabled

# values.yaml
postgresql:
  bundled:
    enabled: false
    auth:
      database: dbbat
      username: dbbat
    primary:
      persistence:
        size: 8Gi
```

### Phase 3: Gateway API Support

Add TCPRoute for Kubernetes Gateway API:

```yaml
gatewayAPI:
  enabled: false
  parentRefs:
    - name: my-gateway
      namespace: gateway-system
```

### Phase 4: CloudNativePG Integration

Create Cluster resource if operator is available:

```yaml
cloudnativepg:
  enabled: false
  instances: 3
  storage:
    size: 10Gi
  backup:
    enabled: true
    schedule: "0 0 * * *"
```

## Files to Create

```
charts/
└── dbbat/
    ├── Chart.yaml
    ├── values.yaml
    ├── values.schema.json
    ├── README.md
    ├── templates/
    │   ├── _helpers.tpl
    │   ├── configmap.yaml
    │   ├── deployment.yaml
    │   ├── hpa.yaml
    │   ├── ingress.yaml
    │   ├── networkpolicy.yaml
    │   ├── NOTES.txt
    │   ├── pdb.yaml
    │   ├── secret.yaml
    │   ├── service.yaml
    │   ├── service-pg.yaml
    │   └── serviceaccount.yaml
    └── .helmignore
```

## Implementation Order

1. Create basic chart structure with Chart.yaml, values.yaml
2. Implement core templates: deployment, service, configmap, secret
3. Add ingress and service-pg templates
4. Implement security templates: serviceaccount, networkpolicy, pdb
5. Add HPA for autoscaling
6. Create values.schema.json for validation
7. Write comprehensive README.md
8. Add chart publishing to release workflow
9. Test deployment on local cluster (kind/minikube)
10. Test on real Kubernetes cluster

## Open Questions

1. **Metrics endpoint**: Should we add a `/api/v1/metrics` endpoint for Prometheus?
2. **Sidecar support**: Should we document common sidecars (log forwarding, etc.)?
3. **CRD-based PostgreSQL**: CloudNativePG requires CRDs - should this be a separate chart or integrated?
4. **Air-gapped support**: Should we document how to use in air-gapped environments?
