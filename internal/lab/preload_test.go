package lab

import (
	"slices"
	"testing"
)

func TestScrapeImages(t *testing.T) {
	rendered := `
apiVersion: apps/v1
kind: Deployment
spec:
  template:
    spec:
      containers:
        - image: gsoci.azurecr.io/giantswarm/muster:5.7.2
        - image: "gsoci.azurecr.io/giantswarm/mcp-kubernetes:1.0.9"
      initContainers:
        - image: 'docker.io/library/postgres:18.3-alpine'
---
kind: ConfigMap
data:
  # a bare word under an image key is config, not a pullable ref
  image: muster
  settings: |
    image: not-yaml-context:but-tagged
---
kind: Pod
spec:
  containers:
    - image: gsoci.azurecr.io/giantswarm/muster:5.7.2
    - image: gsoci.azurecr.io/giantswarm/valkey@sha256:abcdef0123456789
`
	got := scrapeImages(rendered)
	want := []string{
		"docker.io/library/postgres:18.3-alpine",
		"gsoci.azurecr.io/giantswarm/mcp-kubernetes:1.0.9",
		"gsoci.azurecr.io/giantswarm/muster:5.7.2",
		"gsoci.azurecr.io/giantswarm/valkey@sha256:abcdef0123456789",
		"not-yaml-context:but-tagged",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("scrapeImages:\n got  %v\n want %v", got, want)
	}
}
