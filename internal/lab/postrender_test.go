package lab

import (
	"bytes"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/giantswarm/agentlab/internal/config"
)

const postRenderInput = `---
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: muster
spec:
  parentRefs: []
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: muster
  namespace: agent-platform
spec:
  replicas: 1
  template:
    metadata:
      labels: {app: muster}
    spec:
      containers:
        - name: muster
          image: muster:1.0
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: muster-config
data:
  config.yaml: |
    aggregator:
      port: 8090
      oauth:
        server:
          enabled: true
    other:
      keep: value
---
apiVersion: v1
kind: Service
metadata:
  name: muster
spec:
  ports: [{port: 8090}]
---
apiVersion: v1
kind: Service
metadata:
  name: kagent-ui
  namespace: kagent
spec:
  type: NodePort
  ports:
    - port: 8080
      targetPort: 8080
      protocol: TCP
      name: ui
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: backstage
  namespace: agent-platform
spec:
  replicas: 1
  template:
    metadata:
      labels: {app: backstage}
    spec:
      containers:
        - name: backstage
          image: backstage:1.0
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: kagent-controller
  namespace: kagent
spec:
  template:
    spec:
      containers:
        - name: controller
          image: kagent:1.0
`

func TestPostRender(t *testing.T) {
	var out bytes.Buffer
	if err := PostRender(strings.NewReader(postRenderInput), &out, 32000); err != nil {
		t.Fatalf("post-render: %v", err)
	}
	rendered := out.String()

	// Routes pass through untouched: the lab runs the real edge topology now
	// (the retired muster-direct mode used to strip the muster HTTPRoute).
	if !strings.Contains(rendered, "HTTPRoute") {
		t.Errorf("HTTPRoute no longer passes through")
	}
	if strings.Count(rendered, "hostNetwork: true") != 2 {
		t.Errorf("hostNetwork patch should hit exactly muster and backstage:\n%s", rendered)
	}
	for _, want := range []string{
		"hostNetwork: true",
		"dnsPolicy: ClusterFirstWithHostNet",
		"maxSurge: 0",
		"maxUnavailable: 1",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("deployment patch missing %q", want)
		}
	}
	if !strings.Contains(rendered, "kind: Service") {
		t.Errorf("unrelated document dropped")
	}

	// The muster ConfigMap passes through UNTOUCHED: the chart renders
	// allowPublicClientRegistration itself since muster 5.7.2 (muster#1118),
	// so the retired DCR edit must not sneak back in.
	var docs []map[string]any
	dec := yaml.NewDecoder(strings.NewReader(rendered))
	for {
		var doc map[string]any
		if err := dec.Decode(&doc); err != nil {
			break
		}
		docs = append(docs, doc)
	}
	if len(docs) != 7 {
		t.Fatalf("got %d documents, want 7", len(docs))
	}
	for _, doc := range docs {
		if doc["kind"] != "Deployment" {
			continue
		}
		name := doc["metadata"].(map[string]any)["name"]
		podSpec := doc["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
		_, patched := podSpec["hostNetwork"]
		if (name == "muster" || name == "backstage") != patched {
			t.Errorf("hostNetwork patch wrong for deployment %v (patched=%v)", name, patched)
		}
	}
	for _, doc := range docs {
		if doc["kind"] == "Service" {
			name := doc["metadata"].(map[string]any)["name"]
			ports := doc["spec"].(map[string]any)["ports"].([]any)
			nodePort, pinned := ports[0].(map[string]any)["nodePort"]
			switch name {
			case "kagent-ui":
				if !pinned || nodePort != config.KagentUINodePort {
					t.Errorf("kagent-ui nodePort not pinned: %v", ports[0])
				}
			case "muster":
				if pinned {
					t.Errorf("unrelated service gained a nodePort: %v", ports[0])
				}
			}
			continue
		}
		if doc["kind"] != "ConfigMap" {
			continue
		}
		inner := doc["data"].(map[string]any)["config.yaml"].(string)
		var cfg map[string]any
		if err := yaml.Unmarshal([]byte(inner), &cfg); err != nil {
			t.Fatalf("inner config unparsable: %v", err)
		}
		server := cfg["aggregator"].(map[string]any)["oauth"].(map[string]any)["server"].(map[string]any)
		if _, edited := server["allowPublicClientRegistration"]; edited {
			t.Errorf("retired DCR edit resurfaced in the rendered config: %v", server)
		}
		if server["enabled"] != true {
			t.Errorf("chart-rendered server keys lost: %v", server)
		}
		if cfg["other"].(map[string]any)["keep"] != "value" {
			t.Errorf("unrelated config lost: %v", cfg["other"])
		}
	}
}

const dexLocalhostInput = `---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: model-manager
  namespace: agent-platform
spec:
  template:
    spec:
      containers:
        - name: model-manager
          image: model-manager:1.0
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: kagent-controller
spec:
  template:
    spec:
      containers:
        - name: controller
          image: kagent:1.0
`

func TestPostRenderDexLocalhostSidecar(t *testing.T) {
	var out bytes.Buffer
	if err := PostRender(strings.NewReader(dexLocalhostInput), &out, 31000); err != nil {
		t.Fatal(err)
	}
	// Re-render the output: the sidecar must be replaced, not appended.
	var again bytes.Buffer
	if err := PostRender(bytes.NewReader(out.Bytes()), &again, 31000); err != nil {
		t.Fatal(err)
	}
	dec := yaml.NewDecoder(&again)
	var docs []map[string]any
	for {
		var d map[string]any
		if err := dec.Decode(&d); err != nil {
			break
		}
		docs = append(docs, d)
	}
	if len(docs) != 2 {
		t.Fatalf("want 2 documents, got %d", len(docs))
	}
	containersOf := func(d map[string]any) []any {
		return d["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)
	}
	mm := containersOf(docs[0])
	if len(mm) != 2 {
		t.Fatalf("model-manager: want the app container + one sidecar, got %d containers", len(mm))
	}
	side := mm[1].(map[string]any)
	if side["name"] != dexLocalhostContainer || side["image"] != dexLocalhostImage {
		t.Fatalf("unexpected sidecar: %v", side)
	}
	args := side["args"].([]any)
	if args[0] != "TCP6-LISTEN:31000,fork,reuseaddr" || args[1] != "TCP:"+dexServiceAddr {
		t.Fatalf("unexpected sidecar args: %v", args)
	}
	if n := len(containersOf(docs[1])); n != 1 {
		t.Fatalf("kagent-controller must stay untouched, got %d containers", n)
	}
}
