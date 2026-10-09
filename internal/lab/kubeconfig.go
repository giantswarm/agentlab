package lab

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"github.com/giantswarm/agentlab/internal/config"
)

const (
	// nameKey is the "name" field key, shared by the kubeconfig maps and the
	// manifest/JSON lookups elsewhere in the package.
	nameKey = "name"
	// oidcEntryName names the token kubeconfig's user and context entries
	// (and its current-context).
	oidcEntryName = "oidc"
	// labKubeconfigPath is the lab-owned kubeconfig: the kind cluster's admin
	// kubeconfig exactly as kind emits it (its current-context is the
	// kind-<cluster> context), written by the embedded kind at cluster
	// creation and by useClusterKubeconfig on every cluster-facing command,
	// and the one file the embedded Helm and Kubernetes client are built
	// from (restclient.go, kube.go). The one kubeconfig the lab writes —
	// the user's own is never read or merged into. Under StateDir like every
	// other generated artifact; `KUBECONFIG=state/kubeconfig kubectl ...` (or
	// `helm ...`) is how a person looks at the same cluster from a shell.
	labKubeconfigPath = StateDir + "/kubeconfig"
)

// labKubeconfig is labKubeconfigPath made absolute, so a child process reads
// the same file whatever its working directory.
func labKubeconfig() string {
	if abs, err := filepath.Abs(labKubeconfigPath); err == nil {
		return abs
	}
	return labKubeconfigPath
}

// kindKubeconfigCache holds the kubeconfig kind read off the node, per
// process: a docker round trip whose output never changes within a run, while
// up's verification, test and login all want it.
var kindKubeconfigCache struct {
	name string
	raw  []byte
}

// kindKubeconfig returns the cluster's admin kubeconfig from the embedded
// kind, which reads it off the control-plane node (kind.go) — independent of
// any kubeconfig on the host. A cluster that is not running fails here, by
// name, with kind's message.
func kindKubeconfig(clusterName string) ([]byte, error) {
	if kindKubeconfigCache.raw != nil && kindKubeconfigCache.name == clusterName {
		return kindKubeconfigCache.raw, nil
	}
	raw, err := kindKubeconfigRaw(clusterName)
	if err != nil {
		return nil, fmt.Errorf("no kubeconfig for kind cluster %q (is the lab up? `agentlab up`): %w", clusterName, err)
	}
	kindKubeconfigCache.name, kindKubeconfigCache.raw = clusterName, raw
	return kindKubeconfigCache.raw, nil
}

// useClusterKubeconfig exports the kind cluster's kubeconfig to
// labKubeconfigPath. Every command that talks to the cluster calls it first:
// from then on its embedded Helm and its Kubernetes client are deterministic
// about the cluster (the one agentlab.yaml names), and a lab that is not
// running fails right here, by name — never as a call against whatever
// cluster the shell's own kubeconfig happens to point at. The user's
// kubeconfig and current-context are never read or changed; a copy of the
// lab's own file the shell's KUBECONFIG names is refreshed with it
// (refreshKubeconfigCopies).
func useClusterKubeconfig(cfg *config.Config) error {
	raw, err := kindKubeconfig(cfg.ClusterName)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(StateDir, 0o750); err != nil {
		return err
	}
	if err := os.WriteFile(labKubeconfigPath, raw, 0o600); err != nil {
		return err
	}
	refreshKubeconfigCopies(cfg.ClusterName, raw)
	// The embedded client (kube.go) is built from this file: a bundle built
	// before the rewrite must not outlive it.
	resetLabKube()
	return nil
}

// refreshKubeconfigCopies brings the copies of this lab's admin kubeconfig
// that the shell's KUBECONFIG names up to date with the cluster. A copy taken
// before `down` and `up` (a lab lease's, say) still carries the previous
// cluster's CA and fails every call against the new one until it is
// rewritten — kind would have written the new cluster into KUBECONFIG, and
// this keeps that one promise for the copies of the lab's own file. A file
// counts as a copy when every cluster, context and user in it is this lab's
// kind entry: the shell's own kubeconfig, ~/.kube/config and another lab's
// are never written. A copy that already carries the cluster's address and
// CA stays as it is, additions included (a proxy-url, say); one that cannot
// be refreshed is noted, never fatal — the lab itself is fine.
func refreshKubeconfigCopies(clusterName string, fresh []byte) {
	want, ok := readKubeconfigCopy(fresh)
	if !ok || !want.ofLab(clusterName) {
		return
	}
	own := labKubeconfig()
	home, _ := os.UserHomeDir()
	for _, path := range filepath.SplitList(os.Getenv("KUBECONFIG")) {
		abs, err := filepath.Abs(path)
		if path == "" || err != nil || abs == own || (home != "" && abs == filepath.Join(home, ".kube", "config")) {
			continue
		}
		raw, err := os.ReadFile(abs) // #nosec G304 G703 -- a file the shell's KUBECONFIG names, read to see whether it is the lab's own
		if err != nil {
			continue
		}
		got, ok := readKubeconfigCopy(raw)
		if !ok || !got.ofLab(clusterName) || got.endpoint() == want.endpoint() {
			continue
		}
		if err := replaceFile(abs, fresh); err != nil {
			note("the copy of the lab kubeconfig at %s is the previous cluster's and could not be refreshed: %v", abs, err)
			continue
		}
		note("refreshed the copy of the lab kubeconfig at %s for the new cluster", abs)
	}
}

// replaceFile writes data over path owner-only, through a temporary file in
// the same directory and one rename: a reader of the file sees the previous
// content or the new one, never half a file.
func replaceFile(path string, data []byte) error {
	// path is a kubeconfig the shell's KUBECONFIG names, the
	// person's own file, rewritten only when it is a copy of the lab's.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil { // #nosec G703 -- see above
		return err
	}
	if err := os.Rename(tmp, path); err != nil { // #nosec G703 -- see above
		_ = os.Remove(tmp) // #nosec G703 -- see above
		return err
	}
	return nil
}

// kubeconfigCopy is what the refresh reads of a kubeconfig: the names of its
// entries, and the cluster's address and CA.
type kubeconfigCopy struct {
	Clusters []struct {
		Name    string `yaml:"name"`
		Cluster struct {
			Server string `yaml:"server"`
			CA     string `yaml:"certificate-authority-data"`
		} `yaml:"cluster"`
	} `yaml:"clusters"`
	Contexts []struct {
		Name string `yaml:"name"`
	} `yaml:"contexts"`
	Users []struct {
		Name string `yaml:"name"`
	} `yaml:"users"`
}

func readKubeconfigCopy(raw []byte) (kubeconfigCopy, bool) {
	var k kubeconfigCopy
	err := yaml.Unmarshal(raw, &k)
	return k, err == nil && len(k.Clusters) > 0
}

// ofLab reports whether every cluster, context and user is the lab's kind
// entry (kind-<clusterName>, as kind names all three): the admin kubeconfig
// as kind emits it, or a copy of it.
func (k kubeconfigCopy) ofLab(clusterName string) bool {
	entry := "kind-" + clusterName
	if len(k.Clusters) == 0 || len(k.Contexts) == 0 || len(k.Users) == 0 {
		return false
	}
	for _, c := range k.Clusters {
		if c.Name != entry {
			return false
		}
	}
	for _, c := range k.Contexts {
		if c.Name != entry {
			return false
		}
	}
	for _, u := range k.Users {
		if u.Name != entry {
			return false
		}
	}
	return true
}

// endpoint is the cluster's address and CA: the same pair is the same
// cluster, a different one the cluster that replaced it.
func (k kubeconfigCopy) endpoint() [2]string {
	return [2]string{k.Clusters[0].Cluster.Server, k.Clusters[0].Cluster.CA}
}

// kindClusterEntry is the cluster entry (name and server/CA) of the kind
// kubeconfig, for the token-only kubeconfigs that reuse the endpoint without
// the admin client certificate.
func kindClusterEntry(clusterName string) (string, map[string]any, error) {
	raw, err := kindKubeconfig(clusterName)
	if err != nil {
		return "", nil, err
	}
	var kc struct {
		Clusters []struct {
			Name    string         `yaml:"name"`
			Cluster map[string]any `yaml:"cluster"`
		} `yaml:"clusters"`
	}
	if err := yaml.Unmarshal(raw, &kc); err != nil {
		return "", nil, fmt.Errorf("parsing kind kubeconfig: %w", err)
	}
	if len(kc.Clusters) == 0 {
		return "", nil, fmt.Errorf("kind kubeconfig has no clusters")
	}
	return kc.Clusters[0].Name, kc.Clusters[0].Cluster, nil
}

// writeTokenKubeconfig builds a kubeconfig that authenticates ONLY with the
// given bearer token. Needed because the kind kubeconfig ships an admin client
// certificate, and a client cert always wins over --token / a token-based
// user — kubectl would silently keep authenticating as kubernetes-admin.
func writeTokenKubeconfig(cfg *config.Config, token, outPath string) error {
	name, cluster, err := kindClusterEntry(cfg.ClusterName)
	if err != nil {
		return err
	}

	out := map[string]any{
		"apiVersion": "v1",
		"kind":       "Config",
		"clusters": []map[string]any{
			{nameKey: name, "cluster": cluster},
		},
		"users": []map[string]any{
			{nameKey: oidcEntryName, "user": map[string]any{"token": token}},
		},
		"contexts": []map[string]any{
			{nameKey: oidcEntryName, "context": map[string]any{
				"cluster": name, "user": oidcEntryName,
			}},
		},
		"current-context": oidcEntryName,
	}
	data, err := yaml.Marshal(out)
	if err != nil {
		return err
	}
	return os.WriteFile(outPath, data, 0o600)
}

// dropStaleKindContext removes the kind-<clusterName> context, cluster and
// user from the kubeconfigs a shell reads by default (~/.kube/config and the
// files KUBECONFIG names, the lab's own and its pure copies aside — those are
// refreshed) when they point at a cluster that is gone: an earlier kind
// wrote the entry, the host rebooted or the lab was recreated, and the entry
// still carries the old CA, so `kubectl --context kind-<lab>` fails with an
// unknown-authority error that reads like a lab outage. An entry that
// carries the live cluster's address and CA stays; so does every entry that
// is not the lab's. The lab's own kubeconfig (state/kubeconfig, or the
// lease's copy) is the one to use. Never fatal.
func dropStaleKindContext(clusterName string, fresh []byte) {
	want, ok := readKubeconfigCopy(fresh)
	if !ok {
		return
	}
	own := labKubeconfig()
	var paths []string
	if home, err := os.UserHomeDir(); err == nil {
		paths = append(paths, filepath.Join(home, ".kube", "config"))
	}
	paths = append(paths, filepath.SplitList(os.Getenv("KUBECONFIG"))...)
	seen := map[string]bool{}
	for _, path := range paths {
		abs, err := filepath.Abs(path)
		if path == "" || err != nil || abs == own || seen[abs] {
			continue
		}
		seen[abs] = true
		raw, err := os.ReadFile(abs) // #nosec G304 G703 -- a kubeconfig the shell reads by default, read to see whether it holds this lab's stale entry
		if err != nil {
			continue
		}
		if got, ok := readKubeconfigCopy(raw); ok && got.ofLab(clusterName) {
			continue // a pure copy of the lab's file: refreshKubeconfigCopies' to keep current
		}
		pruned, ok := pruneStaleKindEntries(raw, "kind-"+clusterName, want.endpoint())
		if !ok {
			continue
		}
		if err := replaceFile(abs, pruned); err != nil {
			note("the stale kind-%s context in %s could not be removed: %v", clusterName, abs, err)
			continue
		}
		note("removed the stale kind-%s context from %s (use the lab's own kubeconfig)", clusterName, abs)
	}
}

// pruneStaleKindEntries drops the entries named entry from a kubeconfig whose
// cluster of that name is not the live endpoint (server, CA); ok is false
// when there was nothing to drop or the file is not a kubeconfig. The
// current-context goes with its context, and the user stays while another
// context uses it.
func pruneStaleKindEntries(raw []byte, entry string, live [2]string) (pruned []byte, ok bool) {
	var kc map[string]any
	if err := yaml.Unmarshal(raw, &kc); err != nil || kc == nil {
		return nil, false
	}
	stale := false
	clusters := filterEntries(kc["clusters"], func(m map[string]any) bool {
		if m[nameKey] != entry {
			return true
		}
		c, _ := m["cluster"].(map[string]any)
		server, _ := c["server"].(string)
		ca, _ := c["certificate-authority-data"].(string)
		if [2]string{server, ca} == live {
			return true
		}
		stale = true
		return false
	})
	if !stale {
		return nil, false
	}
	kc["clusters"] = clusters
	kc["contexts"] = filterEntries(kc["contexts"], func(m map[string]any) bool {
		c, _ := m["context"].(map[string]any)
		return m[nameKey] != entry && c["cluster"] != entry
	})
	usedUsers := map[any]bool{}
	for _, c := range asEntries(kc["contexts"]) {
		ctx, _ := c["context"].(map[string]any)
		usedUsers[ctx["user"]] = true
	}
	kc["users"] = filterEntries(kc["users"], func(m map[string]any) bool {
		return m[nameKey] != entry || usedUsers[entry]
	})
	if kc["current-context"] == entry {
		kc["current-context"] = ""
	}
	out, err := yaml.Marshal(kc)
	if err != nil {
		return nil, false
	}
	return out, true
}

// asEntries reads a kubeconfig's clusters, contexts or users list as maps.
func asEntries(v any) []map[string]any {
	list, _ := v.([]any)
	out := make([]map[string]any, 0, len(list))
	for _, e := range list {
		if m, ok := e.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// filterEntries keeps the entries of a kubeconfig list that keep returns true
// for.
func filterEntries(v any, keep func(map[string]any) bool) []any {
	out := []any{}
	for _, m := range asEntries(v) {
		if keep(m) {
			out = append(out, m)
		}
	}
	return out
}
