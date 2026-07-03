# Document HTTPRoute (Gateway API) exposure on the website

## Goal

Document the new `httpRoute` chart values on dbbat.com, next to the existing
Ingress documentation in the installation/configuration pages.

## Why

The Helm chart now supports exposing the API/Web UI through a Gateway API
`HTTPRoute` (`charts/dbbat/templates/httproute.yaml`), but the Docusaurus site
(`website/`) only documents the Ingress path. Users discovering the chart via
dbbat.com won't know the Gateway API option exists.

## Implementation

- Add an "HTTPRoute (Gateway API)" section to the Kubernetes/Helm installation
  page under `website/docs/installation/`, mirroring the example in
  `charts/dbbat/README.md` (hostnames, parentRefs, paths, annotations).
- Mention the prerequisites: Gateway API CRDs installed and a Gateway that
  accepts routes from the release namespace; TLS terminated at the Gateway
  listener.

No GitHub issue filed yet — one should be created for this.
