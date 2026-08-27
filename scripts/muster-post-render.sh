#!/usr/bin/env bash
# Helm post-renderer for the agent-platform-standalone install (stdin: the full
# rendered release, stdout: the same with three muster patches). Plain Helm has
# no values hook for any of these, so they live here — the replacement for the
# Flux postRenderers the old agent-platform meta-package forwarded to
# helm-controller.
#
# 1. hostNetwork + dnsPolicy on the muster Deployment: muster must resolve
#    https://localhost:32000/dex to the Dex NodePort from inside the pod so it
#    can share ONE issuer URL with the browser on the Mac. Not a muster chart
#    value.
#
# 2. maxSurge 0: hostNetwork means the pod binds :8090 on the node, so the
#    default rolling update deadlocks on a single-node cluster — the new pod
#    cannot start until the old one releases the port. Old pod goes down first.
#
# 3. allowPublicClientRegistration: WORKAROUND (muster chart <= 5.5.6). The
#    chart exposes the key in values.yaml, values.schema.json and its README,
#    but templates/configmap.yaml never renders it, so the value is silently
#    dropped and Dynamic Client Registration stays gated. Claude Code registers
#    as a PUBLIC client on a random loopback port, so neither
#    `registrationToken` (it cannot send one),
#    `trustedPublicRegistrationRedirectURIs` (the port is random) nor
#    `trustedPublicRegistrationSchemes` (http/https are stripped by mcp-oauth's
#    config validation on purpose) can gate it open. The key is edited into the
#    rendered config surgically — everything else in the ConfigMap stays
#    chart-rendered, so muster bumps via the chart need no hand-copying here.
#    Delete this patch once the chart renders the key.
#
# 4. Drop the muster HTTPRoute. The chart hard-fails on empty
#    ingress.parentRefs in ALL modes (it assumes a public Gateway even for
#    muster-direct), so the values carry a placeholder parentRef and the
#    rendered route is stripped here: this lab has no Gateway, no Gateway API
#    CRDs, and reaches muster through hostNetwork + the kind port mapping.
set -euo pipefail

yq e '
  select(. != null) |
  select((.kind == "HTTPRoute" and .metadata.name == "muster") | not) |
  with(select(.kind == "Deployment" and .metadata.name == "muster");
    .spec.template.spec.hostNetwork = true |
    .spec.template.spec.dnsPolicy = "ClusterFirstWithHostNet" |
    .spec.strategy = {"type": "RollingUpdate",
                      "rollingUpdate": {"maxSurge": 0, "maxUnavailable": 1}}
  ) |
  with(select(.kind == "ConfigMap" and .metadata.name == "muster-config");
    .data["config.yaml"] |= (from_yaml
      | .aggregator.oauth.server.allowPublicClientRegistration = true
      | to_yaml)
  )
'
