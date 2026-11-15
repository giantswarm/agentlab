package lab

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/yaml"

	"github.com/giantswarm/agentlab/internal/config"
)

// A secret source is a reference agentlab.yaml records for a credential
// (aiKey.source, githubToken.source, githubApp's and the workspace
// providers' *Source fields; config.ValidateSecretSource is the rule): never
// the value. `up` and `platform` place each one into the one key of the lab
// Secret its consumer reads, in this process — the reader below hands the
// value over a pipe or the Kubernetes API into memory, and a merge patch
// carries it to the lab's apiserver. It never reaches a process's argv, a
// file, agentlab's output or an error; what is printed is its length.
//
// The reader is chosen by the reference's form:
//
//   - op://<vault>/<item>/<field>: `op read --no-newline <ref>`, the
//     1Password CLI as the person who runs agentlab (its desktop-app
//     integration may ask them to approve the read).
//   - <file>#<dotted.path> (sops:// in front optional): `sops --decrypt
//     --output-type json <file>` with sops' own key discovery (an age
//     identity in ~/.config/sops/age/keys.txt or $SOPS_AGE_KEY_FILE, a KMS,
//     PGP), the path walked in this process.
//   - k8s://<context>/<namespace>/<secret>/<key>[#<dotted.path>]: the key of
//     a Secret in another cluster, read through the embedded client with
//     the named context of the shell's kubeconfig ($KUBECONFIG, else
//     ~/.kube/config) — the one read of that kubeconfig the lab makes.
//
// Where beekeeper (secretTool) is on PATH, the reference is handed to its
// `secret copy` instead, as before agentlab read sources itself: it reads
// the value in its own process under its own vault rules.

// The CLIs a source is read through.
const (
	opBin   = "op"
	sopsBin = "sops"
)

// secretReaderInstall is where each reader CLI comes from.
var secretReaderInstall = map[string]string{
	opBin:   "https://developer.1password.com/docs/cli/get-started/",
	sopsBin: "https://github.com/getsops/sops/releases",
}

// secretSourceTimeout bounds one source's read: the 1Password CLI may wait
// for the person to approve it in the desktop app.
const secretSourceTimeout = 2 * time.Minute

// secretSourceFieldManager is the field manager of a placed key. A merge
// patch under a name of its own (an Update, never an apply) leaves the keys
// agentlab's server-side apply owns in the same Secret — a GitHub App's
// public ids — to that apply, and the apply never prunes the placed key.
const secretSourceFieldManager = "agentlab-secret-source" // #nosec G101 -- a field manager's NAME, not a credential

// secretRef is a parsed secret source.
type secretRef struct {
	op                           string // the op:// reference as recorded
	kubeContext, ns, secret, key string // a k8s:// reference
	file                         string // a SOPS file
	path                         string // the dotted path inside the SOPS file or the k8s:// key
}

// parseSecretSource reads a reference config.ValidateSecretSource accepts.
func parseSecretSource(source string) (secretRef, error) {
	if err := config.ValidateSecretSource(source); err != nil {
		return secretRef{}, fmt.Errorf("%q: %w", source, err)
	}
	if strings.HasPrefix(source, "op://") {
		return secretRef{op: source}, nil
	}
	if rest, ok := strings.CutPrefix(source, "k8s://"); ok {
		target, path, _ := strings.Cut(rest, "#")
		parts := strings.Split(target, "/")
		return secretRef{kubeContext: parts[0], ns: parts[1], secret: parts[2], key: parts[3], path: path}, nil
	}
	file, path, _ := strings.Cut(strings.TrimPrefix(source, "sops://"), "#")
	return secretRef{file: file, path: path}, nil
}

// secretReader is the CLI a source is read through: op, sops, or "" for a
// k8s:// reference, which the embedded client reads.
func secretReader(source string) string {
	switch {
	case strings.HasPrefix(source, "op://"):
		return opBin
	case strings.HasPrefix(source, "k8s://"):
		return ""
	}
	return sopsBin
}

// secretToolOnPath reports whether beekeeper answers on PATH, which then
// places every source; the seam the tests replace.
var secretToolOnPath = func() bool {
	_, err := exec.LookPath(secretTool)
	return err == nil
}

// readSecretSource reads the one value source names, in this process; the
// seam the tests replace.
var readSecretSource = func(ctx context.Context, source string) ([]byte, error) {
	ref, err := parseSecretSource(source)
	if err != nil {
		return nil, err
	}
	switch {
	case ref.op != "":
		return readSecretCLI(ctx, opBin, "read", "--no-newline", ref.op)
	case ref.kubeContext != "":
		return readKubeSource(ctx, ref)
	}
	return readSOPSSource(ctx, ref)
}

// readSecretCLI runs a reader CLI and answers its stdout, the value. Its
// stdin is empty, so a reader that wants a password fails instead of
// waiting on the terminal; a failure carries the command (references only)
// and what the reader said on stderr — never its stdout.
func readSecretCLI(ctx context.Context, name string, args ...string) ([]byte, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...) // #nosec G204 -- a fixed reader CLI; the arguments are references, never values
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return nil, fmt.Errorf("%s is not on PATH: install it (%s) or record the source in another form", name, secretReaderInstall[name])
		}
		if ctx.Err() != nil {
			return nil, fmt.Errorf("%s %s answered nothing within %s", name, strings.Join(args, " "), secretSourceTimeout)
		}
		return nil, cmdError(name, args, err, stderr.Bytes())
	}
	return stdout.Bytes(), nil
}

// readSOPSSource decrypts the SOPS file and answers the value at the path.
func readSOPSSource(ctx context.Context, ref secretRef) ([]byte, error) {
	doc, err := readSecretCLI(ctx, sopsBin, "--decrypt", "--output-type", "json", ref.file)
	if err != nil {
		return nil, err
	}
	v, err := valueAtPath(doc, ref.path)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", ref.file, err)
	}
	return []byte(v), nil
}

// kubeSourceSecretData answers the data of a Secret in the cluster a
// context of the shell's kubeconfig reaches; the seam the tests replace.
var kubeSourceSecretData = func(ctx context.Context, kubeContext, ns, name string) (map[string][]byte, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	restCfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{CurrentContext: kubeContext}).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("context %s of the kubeconfig ($KUBECONFIG, else ~/.kube/config): %w", kubeContext, err)
	}
	cs, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("context %s: %w", kubeContext, err)
	}
	s, err := cs.CoreV1().Secrets(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("context %s: %w", kubeContext, err)
	}
	return s.Data, nil
}

// readKubeSource reads the key of a Secret in another cluster, or with a
// path one value of the YAML or JSON document the key holds.
func readKubeSource(ctx context.Context, ref secretRef) ([]byte, error) {
	data, err := kubeSourceSecretData(ctx, ref.kubeContext, ref.ns, ref.secret)
	if err != nil {
		return nil, err
	}
	raw, ok := data[ref.key]
	if !ok {
		return nil, fmt.Errorf("context %s: secret %s/%s has no key %s", ref.kubeContext, ref.ns, ref.secret, ref.key)
	}
	if ref.path == "" {
		return raw, nil
	}
	doc, err := yaml.YAMLToJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("context %s: secret %s/%s key %s holds no YAML or JSON document to walk %s", ref.kubeContext, ref.ns, ref.secret, ref.key, ref.path)
	}
	v, err := valueAtPath(doc, ref.path)
	if err != nil {
		return nil, fmt.Errorf("context %s: secret %s/%s key %s: %w", ref.kubeContext, ref.ns, ref.secret, ref.key, err)
	}
	return []byte(v), nil
}

// valueAtPath is the one value a dotted path names in a JSON document:
// mappings by key, lists by index. Each step takes the longest run of the
// path's remaining segments the mapping holds as one key, so a key with
// dots in it (credentials.json) resolves unquoted. A failure names the path
// and the shape it met, never a value.
func valueAtPath(doc []byte, path string) (string, error) {
	dec := json.NewDecoder(bytes.NewReader(doc))
	dec.UseNumber()
	var node any
	if err := dec.Decode(&node); err != nil {
		return "", errors.New("not a document a path walks")
	}
	segs := strings.Split(path, ".")
	for i := 0; i < len(segs); {
		at := strings.Join(segs[:i], ".")
		switch n := node.(type) {
		case map[string]any:
			found := false
			for w := len(segs) - i; w >= 1; w-- {
				if child, ok := n[strings.Join(segs[i:i+w], ".")]; ok {
					node, i, found = child, i+w, true
					break
				}
			}
			if !found {
				return "", fmt.Errorf("path %s: no key %q%s", path, segs[i], under(at))
			}
		case []any:
			idx, err := strconv.Atoi(segs[i])
			if err != nil || idx < 0 || idx >= len(n) {
				return "", fmt.Errorf("path %s: %q is no index of the list%s (%d items)", path, segs[i], under(at), len(n))
			}
			node, i = n[idx], i+1
		default:
			return "", fmt.Errorf("path %s: %s holds one value, not a mapping", path, at)
		}
	}
	switch v := node.(type) {
	case string:
		return v, nil
	case json.Number:
		return v.String(), nil
	case bool:
		return strconv.FormatBool(v), nil
	}
	return "", fmt.Errorf("path %s names a mapping, a list or null, not one value", path)
}

// under is " under <at>" for a step below the document's root.
func under(at string) string {
	if at == "" {
		return ""
	}
	return " under " + at
}

// sourcedSecretKey is one key of a lab Secret that a recorded source fills:
// field names the setting and consumer what reads the key, both for the
// messages.
type sourcedSecretKey struct {
	ns, name, key string
	field, source string
	consumer      string
}

// ensureSourcedKey fills the key from its source unless the Secret already
// holds a value there (the Anthropic placeholder counts as none): a lab
// keeps a placed credential across runs without reading the source again,
// and rotating it is deleting the key (or the Secret) and re-running, or
// `agentlab place-secret`. A source that cannot be placed fails the run, so
// a lab whose configuration says where a credential is never comes up
// without it. It reports whether it wrote the key.
func ensureSourcedKey(cfg *config.Config, k sourcedSecretKey) (bool, error) {
	ctx := context.Background()
	held, err := secretKeyHeld(ctx, k.ns, k.name, k.key)
	if err != nil {
		return false, err
	}
	if held {
		note("secret %s/%s key %s already holds a value, left alone (delete it and re-run to place %s again)", k.ns, k.name, k.key, k.source)
		return false, nil
	}
	how, err := placeSecretSource(cfg, k.source, k.ns, k.name, k.key)
	if err != nil {
		return false, fmt.Errorf("secret %s/%s key %s from %s %s: %w\n  %s reads it; once the source answers, re-run `agentlab platform`",
			k.ns, k.name, k.key, k.field, k.source, err, k.consumer)
	}
	if held, err = secretKeyHeld(ctx, k.ns, k.name, k.key); err != nil {
		return false, err
	}
	if !held {
		return false, fmt.Errorf("secret %s/%s key %s: placed from %s %s (%s), but the Secret still lacks a value there", k.ns, k.name, k.key, k.field, k.source, how)
	}
	note("secret %s/%s key %s placed from %s (%s)", k.ns, k.name, k.key, k.source, how)
	return true, nil
}

// placeSecretSource writes the value source names into key of the lab
// Secret ns/name, the Secret created when absent and its other keys kept,
// and answers how, for the note: beekeeper's answer where it is on PATH,
// else the reader and the value's length.
func placeSecretSource(cfg *config.Config, source, ns, name, key string) (string, error) {
	if secretToolOnPath() {
		answer, err := runSecretTool(secretCopyArgs(source, secretTarget(cfg, ns, name, key))...)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("by %s: %s", secretTool, strings.TrimSpace(answer)), nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), secretSourceTimeout)
	defer cancel()
	value, err := readSecretSource(ctx, source)
	if err != nil {
		return "", err
	}
	if len(value) == 0 {
		return "", errors.New("the source names an empty value")
	}
	if err := writeSecretKey(ctx, ns, name, key, value); err != nil {
		return "", err
	}
	reader := secretReader(source)
	if reader == "" {
		reader = "the kubeconfig context " + strings.Split(strings.TrimPrefix(source, "k8s://"), "/")[0]
	}
	return fmt.Sprintf("read through %s, %d bytes; the value stays in agentlab's process", reader, len(value)), nil
}

// writeSecretKey merge-patches one key into the lab Secret ns/name under
// secretSourceFieldManager, creating an Opaque Secret when there is none.
// The value travels in the request body only; an error names the Secret and
// the key.
func writeSecretKey(ctx context.Context, ns, name, key string, value []byte) error {
	k, err := labKube()
	if err != nil {
		return err
	}
	client := k.resource(gvrSecrets, ns)
	data := map[string][]byte{key: value}
	// The patch is the Secret's data alone ([]byte marshals as base64).
	patch, err := json.Marshal(struct {
		Data map[string][]byte `json:"data"`
	}{data})
	if err != nil {
		return err
	}
	secret, err := toUnstructured(&corev1.Secret{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: kindSecret},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Type:       corev1.SecretTypeOpaque,
		Data:       data,
	})
	if err != nil {
		return err
	}
	for range 2 {
		_, err = client.Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{FieldManager: secretSourceFieldManager})
		if !apierrors.IsNotFound(err) {
			break
		}
		_, err = client.Create(ctx, secret, metav1.CreateOptions{FieldManager: secretSourceFieldManager})
		if !apierrors.IsAlreadyExists(err) {
			break
		}
		// Created between the patch and the create: patch it once more.
	}
	if err != nil {
		return fmt.Errorf("writing key %s of %s: %w", key, describe(gvrSecrets, ns, name), err)
	}
	return nil
}

// secretKeyHeld reports whether the lab Secret ns/name holds a value under
// key that is not the lab's placeholder. Nothing of the value is kept or
// printed; the one comparison is with the public placeholder constant.
func secretKeyHeld(ctx context.Context, ns, name, key string) (bool, error) {
	obj, err := getObject(ctx, gvrSecrets, ns, name)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	value, err := secretDataValue(obj, key)
	if err != nil || len(value) == 0 {
		return false, nil
	}
	return !bytes.Equal(value, []byte(placeholderAPIKey)), nil
}

// PlaceSecret is `agentlab place-secret <ref> <namespace>/<secret>/<key>`:
// the value the reference names written into that key of the lab's Secret,
// whether or not it holds one — the way to place a credential agentlab.yaml
// records no source for (the github MCP server's client, the sign-in App's
// client secret) and to rotate one it does.
func PlaceSecret(cfg *config.Config, source, target string) error {
	parts := strings.Split(target, "/")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return fmt.Errorf("target %q: <namespace>/<secret>/<key>", target)
	}
	if _, err := parseSecretSource(source); err != nil {
		return err
	}
	if err := useClusterKubeconfig(cfg); err != nil {
		return err
	}
	ns, name, key := parts[0], parts[1], parts[2]
	step("Placing %s into the Secret %s/%s key %s", source, ns, name, key)
	how, err := placeSecretSource(cfg, source, ns, name, key)
	if err != nil {
		return fmt.Errorf("secret %s/%s key %s from %s: %w", ns, name, key, source, err)
	}
	note("placed (%s); a consumer that reads its environment at start only needs a restart", how)
	return nil
}
