package conncheck

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"

	"github.com/fclairamb/dbbat/internal/proxy/shared"
	"github.com/fclairamb/dbbat/internal/proxy/upstream"
	"github.com/fclairamb/dbbat/internal/store"
)

// checkCluster validates a `protocol: kubernetes` row on its own, in the order
// an operator debugs one: can we reach the API server, does it accept the
// token, and may this ServiceAccount create `pods/portforward` here.
//
// The RBAC question is asked explicitly with a SelfSubjectAccessReview rather
// than inferred from a failed port-forward, because that is the difference
// between "add `create` on `pods/portforward` to the Role" and a 403 buried in
// a stream upgrade error.
func (c *Checker) checkCluster(ctx context.Context, dialer *shared.Dialer, srv *store.Server) Result {
	tunnel, err := dialer.ConnectKubernetes(ctx, c.resolver, c.encryptionKey, srv.UID)
	if err != nil {
		return classifyKubernetesError(err, StageClusterAPI)
	}

	allowed, reason, err := tunnel.PortForwardAllowed(ctx)
	if err != nil {
		return classifyKubernetesError(err, StageClusterAPI)
	}

	if !allowed {
		message := "the service account may not create pods/portforward in namespace " + tunnel.Namespace() +
			" — grant `create` on `pods/portforward` in a Role bound to it (see docs/kubernetes.md)"
		if reason != "" {
			message += "; the API server said: " + sanitizeText(reason)
		}

		return Result{
			Stage:   StageClusterRBAC,
			Code:    CodeK8sForbidden,
			Message: message,
		}
	}

	return Result{
		OK:    true,
		Stage: StageClusterRBAC,
		Code:  CodeOK,
		Message: "cluster reachable, the service account token was accepted, and it may port-forward in namespace " +
			tunnel.Namespace(),
	}
}

// checkKubernetesTarget resolves a database target that hangs off a cluster row
// before any dial is attempted, so "wrong pod name" and "pod not up" are told
// apart from "the database refused us".
func (c *Checker) checkKubernetesTarget(
	ctx context.Context,
	dialer *shared.Dialer,
	srv *store.Server,
	clusterUID uuid.UUID,
) *Result {
	tunnel, err := dialer.ConnectKubernetes(ctx, c.resolver, c.encryptionKey, clusterUID)
	if err != nil {
		res := classifyKubernetesError(err, StageClusterAPI)

		return &res
	}

	if allowed, reason, err := tunnel.PortForwardAllowed(ctx); err != nil {
		res := classifyKubernetesError(err, StageClusterAPI)

		return &res
	} else if !allowed {
		message := "the service account may not create pods/portforward in namespace " + tunnel.Namespace() +
			" — grant `create` on `pods/portforward` in a Role bound to it (see docs/kubernetes.md)"
		if reason != "" {
			message += "; the API server said: " + sanitizeText(reason)
		}

		return &Result{Stage: StageClusterRBAC, Code: CodeK8sForbidden, Message: message}
	}

	podName, err := tunnel.ResolvePod(ctx, srv.Host)
	if err != nil {
		res := classifyKubernetesError(err, StageClusterTarget)

		return &res
	}

	if err := tunnel.CheckPodReady(ctx, podName); err != nil {
		res := classifyKubernetesError(err, StageClusterTarget)
		// Naming the resolved pod matters when the host was a service: the
		// admin typed `svc/postgres` and needs to know which pod that became.
		if podName != srv.Host {
			res.Message += " (resolved from " + srv.Host + ")"
		}

		return &res
	}

	// Everything the cluster can tell us checks out; the caller carries on with
	// the ordinary dial + database login.
	return nil
}

// isKubernetesStage reports whether a target dial failure originates from the
// cluster tunnel rather than from the database behind it.
func isKubernetesStage(err error) bool {
	for _, sentinel := range []error{
		upstream.ErrKubernetesNoToken,
		upstream.ErrKubernetesNoCACert,
		upstream.ErrKubernetesUnauthorized,
		upstream.ErrKubernetesForbidden,
		upstream.ErrKubernetesTargetNotFound,
		upstream.ErrKubernetesTargetNotReady,
		shared.ErrKubernetesViaUnsupported,
	} {
		if errors.Is(err, sentinel) {
			return true
		}
	}

	return strings.Contains(err.Error(), "kubernetes tunnel") ||
		strings.Contains(err.Error(), "failed to load kubernetes cluster")
}

// classifyKubernetesError maps the upstream package's sentinels onto stages and
// codes. It deliberately matches sentinels rather than text: the whole reason
// internal/proxy/upstream classifies its own failures is so this package never
// has to import client-go to tell a dead token from a missing verb.
func classifyKubernetesError(err error, stage Stage) Result {
	switch {
	case errors.Is(err, upstream.ErrKubernetesNoToken):
		return Result{
			Stage:   StageConfig,
			Code:    CodeNoAuthMethod,
			Message: "the cluster row has no service account token: paste one from the token Secret (see docs/kubernetes.md)",
		}
	case errors.Is(err, upstream.ErrKubernetesNoCACert):
		return Result{
			Stage: StageConfig,
			Code:  CodeMissingConfig,
			Message: "the cluster row pins no CA certificate — paste the API server's CA bundle " +
				"(the token Secret's ca.crt key), or explicitly enable \"skip TLS verification\". " +
				"dbbat will not silently fall back to this host's system trust store for the " +
				"connection that carries the service account token",
		}
	case errors.Is(err, upstream.ErrKubernetesUnauthorized):
		return Result{
			Stage: StageClusterAuth,
			Code:  CodeAuthRejected,
			Message: "the API server rejected the service account token: it expired, was revoked, " +
				"or belongs to a different cluster",
		}
	case errors.Is(err, upstream.ErrKubernetesForbidden):
		return Result{
			Stage: StageClusterRBAC,
			Code:  CodeK8sForbidden,
			Message: "the service account authenticated but RBAC denied the action — " +
				"check the Role and RoleBinding (see docs/kubernetes.md): " + sanitize(err),
		}
	case errors.Is(err, upstream.ErrKubernetesTargetNotFound):
		return Result{
			Stage: StageClusterTarget,
			Code:  CodeK8sTargetNotFound,
			Message: "no such pod or service in the cluster row's namespace — the host field addresses a pod " +
				"(`<pod-name>`) or a service (`svc/<name>`) inside that namespace, never an arbitrary host",
		}
	case errors.Is(err, upstream.ErrKubernetesTargetNotReady):
		return Result{
			Stage:   StageClusterTarget,
			Code:    CodeK8sTargetNotReady,
			Message: "the target exists but no ready pod backs it: " + sanitize(err),
		}
	case errors.Is(err, shared.ErrViaNotTunnel):
		return Result{
			Stage:   StageConfig,
			Code:    CodeViaNotSSH,
			Message: "via_uid does not reference a tunnel server (ssh or kubernetes)",
		}
	case errors.Is(err, shared.ErrKubernetesViaUnsupported):
		return Result{
			Stage: StageConfig,
			Code:  CodeViaNotSSH,
			Message: "an SSH bastion cannot itself be reached through a kubernetes tunnel; " +
				"the supported nesting is the other way round",
		}
	}

	// Anything left is a transport problem reaching the API server: DNS, a
	// firewall, a TLS mismatch against the pasted CA bundle.
	res := classifyNetworkError(err, stage)
	if res.Code == CodeInternal {
		if isTLSTrustFailure(err) {
			return Result{
				Stage: stage,
				Code:  CodeDBHandshakeFailed,
				Message: "the API server's certificate was not trusted: paste the cluster CA bundle into " +
					"the CA certificate field (`kubectl get secret <token-secret> -o jsonpath='{.data.ca\\.crt}' | base64 -d`)",
			}
		}

		res.Message = "could not reach the Kubernetes API server: " + sanitize(err)
	}

	return res
}

// isTLSTrustFailure reports whether err is certificate verification failing,
// which on this path almost always means a missing or wrong CA bundle.
func isTLSTrustFailure(err error) bool {
	msg := strings.ToLower(err.Error())

	return strings.Contains(msg, "certificate signed by unknown authority") ||
		strings.Contains(msg, "x509:") ||
		strings.Contains(msg, "tls: failed to verify")
}

// sanitizeText bounds a server-supplied string for display, mirroring sanitize
// for values that are not errors.
func sanitizeText(s string) string {
	const maxLen = 300

	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
	if len(s) > maxLen {
		s = s[:maxLen] + "…"
	}

	return s
}
