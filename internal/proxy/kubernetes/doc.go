// Package kubernetes holds the end-to-end integration suite for the Kubernetes
// port-forward tunnel: a real k3s cluster, a real PostgreSQL pod, and dbbat's
// own proxy dialing through `pods/portforward`.
//
// It lives in its own package rather than as another file in
// internal/proxy/upstream because the assertions reach across layers the
// upstream package sits *below* — the PostgreSQL proxy and
// internal/proxy/conncheck both import upstream, so an in-package test there
// would be an import cycle.
//
// Nothing here imports k8s.io/client-go. That dependency stays confined to
// internal/proxy/upstream/kubernetes*.go (see docs/kubernetes.md); the cluster
// is driven with `kubectl` inside the k3s container instead.
//
// Everything of substance is behind the `integration` build tag, so a plain
// `go test ./...` neither compiles nor links it. This file carries no tag
// purely so the package still exists — and therefore still builds — in a bare
// `go build ./...`, mirroring internal/proxy/testsupport.
package kubernetes
