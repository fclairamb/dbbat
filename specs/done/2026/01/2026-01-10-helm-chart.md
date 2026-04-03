# Helm Chart for DBBat

## Status: Implemented

**GitHub Issue:** [#7](https://github.com/fclairamb/dbbat/issues/7)

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

Charts will be stored as OCI artifacts in GHCR alongside container images:

```bash
# Install from OCI registry
helm install dbbat oci://ghcr.io/fclairamb/charts/dbbat --version 1.0.0
```

**Rationale:**
- Single registry for both images and charts
- No additional infrastructure needed
- Native Helm 3.8+ support (widely adopted since 2022)
- Versioned alongside the application

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
    │   ├── pdb.yaml                  # PodDisruptionBudget (optional)
    │   ├── networkpolicy.yaml        # Network policies (optional)
    │   └── NOTES.txt
    ├── values.schema.json            # JSON Schema for values validation
    └── README.md
```

## Architecture Decisions

### Decision 1: PostgreSQL Deployment Strategy

DBBat requires PostgreSQL for its internal storage. Users must provide an external PostgreSQL DSN - no bundled database.

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

**Rationale:**
- Simplest chart with no stateful complexity
- Users manage their own database (RDS, Cloud SQL, managed PostgreSQL)
- Follows "cattle not pets" philosophy
- Production deployments should use managed PostgreSQL anyway

### Decision 2: Secret Management

Support both inline secrets and external secret references, with external secrets taking precedence:

```yaml
secrets:
  # Option 1: Create secret from values (simple, for getting started)
  encryptionKey: ""      # Base64-encoded AES-256 key
  databasePassword: ""

  # Option 2: Use existing secret (takes precedence, for production/GitOps)
  existingSecret: ""
  keys:
    encryptionKey: "DBB_KEY"
    databasePassword: "DB_PASSWORD"
```

**Rationale:**
- Inline secrets are simple for getting started
- External secrets enable GitOps workflows and integration with Vault, External Secrets Operator, etc.
- Supporting both provides flexibility without complexity

### Decision 3: PostgreSQL Proxy Service

The PostgreSQL proxy (port 5432) uses TCP, not HTTP. Default to **ClusterIP** for security, with options to change to LoadBalancer or NodePort for external access.

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

**Rationale:**
- ClusterIP is most secure - only accessible within the cluster
- Users can override to LoadBalancer/NodePort when needed
- Separate `service-pg.yaml` template provides clear separation from API service

### Decision 4: Replica Count and Session Handling

DBBat's PostgreSQL proxy maintains TCP connections. Multiple replicas are supported for high availability:

- **PostgreSQL connections are long-lived**: Client connects once, issues many queries
- **No session affinity needed**: Each connection is independent
- **Load balancing works**: New connections distributed across replicas
- **Clients handle reconnection**: PostgreSQL clients reconnect automatically on pod restart

```yaml
replicaCount: 1

podDisruptionBudget:
  enabled: false
  minAvailable: 1
  # OR maxUnavailable: 1
```

**Note:** HorizontalPodAutoscaler (HPA) is not included. Users who need autoscaling can create their own HPA resource externally.

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

The Helm chart is automatically published to GHCR on every release, alongside container images.

### Publishing Strategy

| Trigger | Chart Version | Container Tag | Published To |
|---------|--------------|---------------|--------------|
| Tag `v1.0.0` | `1.0.0` | `1.0.0`, `latest` | `oci://ghcr.io/fclairamb/charts/dbbat` |
| Tag `v1.0.0-rc.1` | `1.0.0-rc.1` | `1.0.0-rc.1` | `oci://ghcr.io/fclairamb/charts/dbbat` |
| Commit to `main` | `0.0.0-dev.{sha}` | `edge` | `oci://ghcr.io/fclairamb/charts/dbbat` (test releases) |

**Versioning:**
- **Stable releases** (`v1.0.0`): Chart version matches tag (without `v` prefix)
- **Pre-releases** (`v1.0.0-rc.1`): Chart version includes pre-release suffix
- **Test releases** (commits to `main`): Chart version is `0.0.0-dev.{short-sha}`, appVersion is `edge`

### Integration with Release Workflow

Add to `.github/workflows/release.yml`:

```yaml
  publish-chart:
    runs-on: ubuntu-latest
    needs: [release]  # After container images are pushed
    permissions:
      contents: read
      packages: write
    steps:
      - uses: actions/checkout@v6.0.1

      - name: Install Helm
        uses: azure/setup-helm@v4
        with:
          version: v3.17.0

      - name: Login to GHCR
        run: |
          echo "${{ secrets.GITHUB_TOKEN }}" | helm registry login ghcr.io -u ${{ github.actor }} --password-stdin

      - name: Determine version
        id: version
        run: |
          if [[ "${{ github.ref_type }}" == "tag" ]]; then
            # Strip 'v' prefix from tag
            VERSION=${GITHUB_REF_NAME#v}
            APP_VERSION=$VERSION
            echo "version=$VERSION" >> $GITHUB_OUTPUT
            echo "appVersion=$APP_VERSION" >> $GITHUB_OUTPUT
            echo "is_release=true" >> $GITHUB_OUTPUT
          else
            # Development build from main branch
            SHORT_SHA=$(git rev-parse --short HEAD)
            VERSION="0.0.0-dev.${SHORT_SHA}"
            APP_VERSION="edge"
            echo "version=$VERSION" >> $GITHUB_OUTPUT
            echo "appVersion=$APP_VERSION" >> $GITHUB_OUTPUT
            echo "is_release=false" >> $GITHUB_OUTPUT
          fi

      - name: Update Chart.yaml
        run: |
          sed -i "s/^version:.*/version: ${{ steps.version.outputs.version }}/" charts/dbbat/Chart.yaml
          sed -i "s/^appVersion:.*/appVersion: \"${{ steps.version.outputs.appVersion }}\"/" charts/dbbat/Chart.yaml
          cat charts/dbbat/Chart.yaml

      - name: Package chart
        run: |
          helm package charts/dbbat

      - name: Push chart to GHCR
        run: |
          helm push dbbat-*.tgz oci://ghcr.io/fclairamb/charts

      - name: Create release note
        if: steps.version.outputs.is_release == 'true'
        run: |
          echo "📦 Helm chart published: oci://ghcr.io/fclairamb/charts/dbbat:${{ steps.version.outputs.version }}"
          echo ""
          echo "Install with:"
          echo "  helm install dbbat oci://ghcr.io/fclairamb/charts/dbbat --version ${{ steps.version.outputs.version }}"
```

### Testing Chart Releases

For commits to `main` (test releases), the chart can be installed with:

```bash
# List available versions
helm search repo dbbat --versions --devel

# Install development version
helm install dbbat oci://ghcr.io/fclairamb/charts/dbbat --version 0.0.0-dev.abc1234 --devel
```

The `--devel` flag is required for installing development versions.

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
5. Create values.schema.json for validation
6. Write comprehensive README.md
7. Add chart publishing to release workflow (both stable and test releases)
8. Test deployment on local cluster (kind/minikube)
9. Verify test release publishing on commit to main
10. Test stable release publishing with a pre-release tag
11. Test on real Kubernetes cluster

## Open Questions

1. **Metrics endpoint**: Should we add a `/api/v1/metrics` endpoint for Prometheus?
2. **Sidecar support**: Should we document common sidecars (log forwarding, etc.)?
3. **CRD-based PostgreSQL**: CloudNativePG requires CRDs - should this be a separate chart or integrated?
4. **Air-gapped support**: Should we document how to use in air-gapped environments?
