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

## Implementation Plan

1. Add an `## HTTPRoute (Gateway API)` section to
   `website/docs/installation/kubernetes.md`, right after the existing
   `## Ingress` section (and its Traefik variant), matching the page's
   manifest-first style:
   - Intro line presenting it as an alternative to Ingress, with the
     prerequisites (Gateway API CRDs installed, a Gateway accepting routes
     from the release namespace, TLS terminated at the Gateway listener —
     no certificate/TLS config on the route side).
   - Raw `HTTPRoute` manifest mirroring what
     `charts/dbbat/templates/httproute.yaml` renders, using the same example
     values as `charts/dbbat/README.md` (hostname `dbbat.example.com`,
     parentRef `http-gateway`/`istio-gateway`/`https`, path `/`,
     `external-dns` annotation) and backing onto the page's `dbbat` Service
     API port (`4200`).
   - A `### With the Helm chart` subsection showing the `httpRoute.*` values
     block from the chart README (`enabled`, `hostnames`, `parentRefs`,
     `paths`, `annotations`, with `ingress.enabled: false`).
2. Mention the Gateway API implementation as an alternative to an Ingress
   controller in the page's Prerequisites list.
3. QA: `bun run build` from `website/` must pass (catches broken
   links/anchors).
