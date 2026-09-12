package lab

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// A cluster without the certificates.k8s.io/v1beta1 API is refused before
// anything installs, with the only fix — a new cluster — spelled out.
func TestSubstratePreflightWithoutTheAPI(t *testing.T) {
	newFakeLab(t)
	err := preflightPodCertificateAPI(context.Background())
	if err == nil || !strings.Contains(err.Error(), "agentlab down && agentlab up") {
		t.Errorf("want the recreate hint, got %v", err)
	}
}

// testRegistryFlag is the lab registry rewrite the values render ahead of
// the image-cache policy (registryFlag on the default config).
const testRegistryFlag = "--localhost-registry-replacement=agentlab-registry:5000"

// anySlice is a []string as the YAML decoder hands a list back.
func anySlice(s []string) []any {
	out := make([]any, len(s))
	for i, v := range s {
		out[i] = v
	}
	return out
}

func TestAteletArgsMissing(t *testing.T) {
	ds := func(args ...string) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{
			fieldSpec: map[string]any{"template": map[string]any{fieldSpec: map[string]any{
				"containers": []any{
					map[string]any{nameKey: "sidecar", argsKey: anySlice(ateletImageCacheArgs)},
					map[string]any{nameKey: ateletDaemonSet, argsKey: anySlice(args)},
				},
			}}},
		}}
	}
	registry := testRegistryFlag
	// The rendered list: the registry flag, then the policy.
	if got := ateletArgsMissing(ds(append([]string{registry}, ateletImageCacheArgs...)...)); len(got) != 0 {
		t.Errorf("a DaemonSet carrying the policy: missing = %v, want none", got)
	}
	// A platform.valuesFiles overlay that replaced the list: every policy flag
	// is reported (another container's args do not count), in policy order.
	if got := ateletArgsMissing(ds(registry)); !reflect.DeepEqual(got, ateletImageCacheArgs) {
		t.Errorf("a DaemonSet without the policy: missing = %v, want %v", got, ateletImageCacheArgs)
	}
	// A partial list names what is left.
	if got := ateletArgsMissing(ds(registry, ateletImageCacheArgs[0])); !reflect.DeepEqual(got, ateletImageCacheArgs[1:]) {
		t.Errorf("a DaemonSet with one policy flag: missing = %v, want %v", got, ateletImageCacheArgs[1:])
	}
}
