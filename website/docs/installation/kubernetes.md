---
sidebar_position: 4
---

# Kubernetes Deployment

Deploy DBBat on Kubernetes with a Deployment, Service, and Ingress.

## Prerequisites

- Kubernetes cluster (1.19+)
- kubectl configured
- PostgreSQL database accessible from the cluster
- Ingress controller installed (e.g., nginx-ingress, traefik)

## Encryption Key Management

DBBat requires a 32-byte AES-256 encryption key to encrypt database credentials at rest. Proper key management is critical for security.

### Generating the Key

Generate a cryptographically secure 32-byte key:

```bash
openssl rand -base64 32
```

This produces a base64-encoded string like: `K7gNU3sdo+OL0wNhqoVWhr3g6s1xYv72ol/pe/Unols=`

### Creating a Kubernetes Secret

Store the encryption key as a Kubernetes Secret:

```bash
# From a generated key
kubectl create secret generic dbbat-key \
  --from-literal=encryption-key='YOUR_BASE64_KEY_HERE'

# Or from a file
openssl rand 32 > dbbat.key
kubectl create secret generic dbbat-key \
  --from-file=encryption-key=dbbat.key
rm dbbat.key  # Remove local copy
```

For production, use a declarative approach with sealed-secrets, SOPS, or your secrets management solution:

```yaml
# secret.yaml (encrypt this file before committing!)
apiVersion: v1
kind: Secret
metadata:
  name: dbbat-key
  namespace: dbbat
type: Opaque
stringData:
  encryption-key: "K7gNU3sdo+OL0wNhqoVWhr3g6s1xYv72ol/pe/Unols="
```

### Key Security Best Practices

1. **Never commit plaintext keys** - Use sealed-secrets, SOPS, Vault, or external secrets operators
2. **Rotate keys periodically** - Plan for key rotation (requires re-encrypting stored credentials)
3. **Limit access** - Use RBAC to restrict who can read the secret
4. **Use namespaces** - Deploy DBBat in its own namespace with restricted access
5. **Enable encryption at rest** - Ensure your cluster encrypts etcd data

```yaml
# RBAC to restrict secret access
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: dbbat-secret-reader
  namespace: dbbat
rules:
- apiGroups: [""]
  resources: ["secrets"]
  resourceNames: ["dbbat-key"]
  verbs: ["get"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: dbbat-secret-reader-binding
  namespace: dbbat
subjects:
- kind: ServiceAccount
  name: dbbat
  namespace: dbbat
roleRef:
  kind: Role
  name: dbbat-secret-reader
  apiGroup: rbac.authorization.k8s.io
```

## Namespace

Create a dedicated namespace:

```bash
kubectl create namespace dbbat
```

## Database Secret

Store the PostgreSQL connection string:

```bash
kubectl create secret generic dbbat-db \
  --namespace dbbat \
  --from-literal=dsn='postgres://user:password@postgres-host:5432/dbbat?sslmode=require'
```

## Deployment

```yaml
# deployment.yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: dbbat
  namespace: dbbat
  labels:
    app: dbbat
spec:
  replicas: 1  # Single replica recommended for proxy consistency
  selector:
    matchLabels:
      app: dbbat
  template:
    metadata:
      labels:
        app: dbbat
    spec:
      serviceAccountName: dbbat
      containers:
      - name: dbbat
        image: ghcr.io/fclairamb/dbbat:latest
        ports:
        - name: postgres
          containerPort: 5432
          protocol: TCP
        - name: api
          containerPort: 8080
          protocol: TCP
        env:
        - name: DBB_DSN
          valueFrom:
            secretKeyRef:
              name: dbbat-db
              key: dsn
        - name: DBB_KEY
          valueFrom:
            secretKeyRef:
              name: dbbat-key
              key: encryption-key
        - name: DBB_LISTEN_PG
          value: ":5432"
        - name: DBB_LISTEN_API
          value: ":8080"
        resources:
          requests:
            memory: "32Mi"
            cpu: "10m"
          limits:
            memory: "128Mi"
            cpu: "500m"
        livenessProbe:
          httpGet:
            path: /api/v1/health
            port: api
          initialDelaySeconds: 10
          periodSeconds: 30
        readinessProbe:
          httpGet:
            path: /api/v1/health
            port: api
          initialDelaySeconds: 5
          periodSeconds: 10
        securityContext:
          runAsNonRoot: true
          runAsUser: 1000
          readOnlyRootFilesystem: true
          allowPrivilegeEscalation: false
---
apiVersion: v1
kind: ServiceAccount
metadata:
  name: dbbat
  namespace: dbbat
```

## Service

Expose DBBat within the cluster:

```yaml
# service.yaml
apiVersion: v1
kind: Service
metadata:
  name: dbbat
  namespace: dbbat
  labels:
    app: dbbat
spec:
  selector:
    app: dbbat
  ports:
  - name: postgres
    port: 5432
    targetPort: postgres
    protocol: TCP
  - name: api
    port: 8080
    targetPort: api
    protocol: TCP
  type: ClusterIP
```

## Ingress

Expose the REST API externally:

```yaml
# ingress.yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: dbbat
  namespace: dbbat
  annotations:
    nginx.ingress.kubernetes.io/ssl-redirect: "true"
    cert-manager.io/cluster-issuer: "letsencrypt-prod"  # If using cert-manager
spec:
  ingressClassName: nginx
  tls:
  - hosts:
    - dbbat.example.com
    secretName: dbbat-tls
  rules:
  - host: dbbat.example.com
    http:
      paths:
      - path: /
        pathType: Prefix
        backend:
          service:
            name: dbbat
            port:
              name: api
```

### Ingress for Traefik

```yaml
# ingress-traefik.yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: dbbat
  namespace: dbbat
  annotations:
    traefik.ingress.kubernetes.io/router.tls: "true"
spec:
  ingressClassName: traefik
  tls:
  - hosts:
    - dbbat.example.com
    secretName: dbbat-tls
  rules:
  - host: dbbat.example.com
    http:
      paths:
      - path: /
        pathType: Prefix
        backend:
          service:
            name: dbbat
            port:
              name: api
```

## Exposing the PostgreSQL Proxy

The PostgreSQL proxy port (5432) typically cannot be exposed via standard HTTP Ingress. Options include:

### Option 1: LoadBalancer Service

```yaml
# service-lb.yaml
apiVersion: v1
kind: Service
metadata:
  name: dbbat-postgres
  namespace: dbbat
  labels:
    app: dbbat
spec:
  selector:
    app: dbbat
  ports:
  - name: postgres
    port: 5432
    targetPort: postgres
  type: LoadBalancer
```

### Option 2: NodePort Service

```yaml
# service-nodeport.yaml
apiVersion: v1
kind: Service
metadata:
  name: dbbat-postgres
  namespace: dbbat
spec:
  selector:
    app: dbbat
  ports:
  - name: postgres
    port: 5432
    targetPort: postgres
    nodePort: 30432  # Access via any node IP:30432
  type: NodePort
```

### Option 3: TCP Ingress (nginx-ingress)

Configure TCP passthrough in your nginx-ingress controller's ConfigMap:

```yaml
# tcp-services ConfigMap for nginx-ingress
apiVersion: v1
kind: ConfigMap
metadata:
  name: tcp-services
  namespace: ingress-nginx
data:
  "5432": "dbbat/dbbat:5432"
```

## Complete Deployment

Apply all manifests:

```bash
kubectl apply -f namespace.yaml
kubectl apply -f secret.yaml        # Or use your secrets management
kubectl apply -f deployment.yaml
kubectl apply -f service.yaml
kubectl apply -f ingress.yaml
```

Verify the deployment:

```bash
kubectl get pods -n dbbat
kubectl get svc -n dbbat
kubectl get ingress -n dbbat

# Check logs
kubectl logs -n dbbat -l app=dbbat

# Test health endpoint
kubectl port-forward -n dbbat svc/dbbat 8080:8080
curl http://localhost:8080/api/v1/health
```

## High Availability Considerations

For production deployments:

1. **Database**: Use a managed PostgreSQL service (RDS, Cloud SQL) or a PostgreSQL operator
2. **Replicas**: While you can run multiple replicas, consider connection routing implications
3. **Persistence**: DBBat is stateless; all state is in PostgreSQL
4. **Monitoring**: Add Prometheus annotations for metrics scraping

```yaml
# Add to deployment.yaml pod template
metadata:
  annotations:
    prometheus.io/scrape: "true"
    prometheus.io/port: "8080"
    prometheus.io/path: "/api/v1/health"
```

## External Secrets Operator

For production, consider using External Secrets Operator to sync secrets from Vault, AWS Secrets Manager, etc.:

```yaml
apiVersion: external-secrets.io/v1beta1
kind: ExternalSecret
metadata:
  name: dbbat-key
  namespace: dbbat
spec:
  refreshInterval: 1h
  secretStoreRef:
    kind: ClusterSecretStore
    name: vault-backend
  target:
    name: dbbat-key
  data:
  - secretKey: encryption-key
    remoteRef:
      key: secret/dbbat
      property: encryption-key
```

## Next Steps

- [Configure databases](/docs/configuration/databases)
- [Set up access control](/docs/features/access-control)
- [API documentation](/docs/api)
