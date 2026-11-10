package lab

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"filippo.io/age"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

// The Flux half of the credentials part: the merged pull request applied by
// the organization's own Kustomization, which agent-platform-operator keeps
// in commit mode (--flux-namespace, --flux-source-name) with
// spec.decryption.provider sops and the installation's SOPS key Secret
// (--flux-decryption-secret). The key is an age identity the lab generates
// once and keeps under certs/, as it keeps agent-manager's broker client
// secret; Flux reads an age identity from a Secret key ending in .agekey
// (Flux "Kustomization" docs, "Decryption").

const (
	// credentialsAgeDir keeps the lab's age identity.
	credentialsAgeDir = "certs/organization-credentials-sops" // #nosec G101 -- a directory, not a credential
	// credentialsAgeKey is the identity's file and the Secret's key.
	credentialsAgeKey = "age.agekey"
	// credentialsFluxWait bounds Flux fetching and applying the merge.
	credentialsFluxWait = 10 * time.Minute
)

// ensureCredentialsAgeIdentity returns the lab's age identity, generated
// into credentialsAgeDir on first use and kept.
func ensureCredentialsAgeIdentity() (*age.X25519Identity, string, error) {
	if err := os.MkdirAll(credentialsAgeDir, 0o750); err != nil {
		return nil, "", err
	}
	path := filepath.Join(credentialsAgeDir, credentialsAgeKey)
	raw, err := os.ReadFile(path) // #nosec G304 -- the lab's own key path
	if errors.Is(err, os.ErrNotExist) {
		identity, err := age.GenerateX25519Identity()
		if err != nil {
			return nil, "", err
		}
		if err := os.WriteFile(path, []byte(identity.String()+"\n"), 0o600); err != nil {
			return nil, "", err
		}
		fmt.Printf("Generated the organizations' SOPS age identity in %s\n", credentialsAgeDir)
		return identity, path, nil
	}
	if err != nil {
		return nil, "", err
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, "", err
	}
	if err := ageKeyFileMode(info.Mode()); err != nil {
		return nil, "", fmt.Errorf("%s: %w", path, err)
	}
	identity, err := parseAgeIdentity(raw)
	if err != nil {
		return nil, "", fmt.Errorf("%s: %w", path, err)
	}
	return identity, path, nil
}

// ageKeyFileMode refuses an age key file its group or others may read or
// write.
func ageKeyFileMode(mode fs.FileMode) error {
	if perm := mode.Perm(); perm&0o077 != 0 {
		return fmt.Errorf("mode %04o lets its group or others reach the key: chmod 600 it", perm)
	}
	return nil
}

// parseAgeIdentity reads one X25519 identity from an age key file, comment
// lines included, as age-keygen writes it.
func parseAgeIdentity(raw []byte) (*age.X25519Identity, error) {
	identities, err := age.ParseIdentities(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	if len(identities) != 1 {
		return nil, errors.New("want one X25519 identity, as age-keygen writes")
	}
	identity, ok := identities[0].(*age.X25519Identity)
	if !ok {
		return nil, errors.New("want one X25519 identity, as age-keygen writes")
	}
	return identity, nil
}

// placeCredentialsAgeSecret writes the age identity into the Secret the
// operator's Kustomizations decrypt with. A Secret of that name the lab did
// not write is the installation's own key and is left alone.
func placeCredentialsAgeSecret(ctx context.Context, ns, name, path string) error {
	k, err := labKube()
	if err != nil {
		return err
	}
	existing, err := k.clientset.CoreV1().Secrets(ns).Get(ctx, name, metav1.GetOptions{})
	switch {
	case err == nil && existing.Labels[managedByLabel] != managedByAgentlabValue:
		return fmt.Errorf("the SOPS key Secret %s/%s exists and is not agentlab's: the proof does not replace an installation's key", ns, name)
	case err != nil && !apierrors.IsNotFound(err):
		return err
	}
	raw, err := os.ReadFile(path) // #nosec G304 -- the lab's own key path
	if err != nil {
		return err
	}
	_, err = applyTyped(ctx, &corev1.Secret{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: kindSecret},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: map[string]string{managedByLabel: managedByAgentlabValue}},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{credentialsAgeKey: raw},
	})
	return err
}

// readSOPSConfig reads the repository's .sops.yaml at ref with the person's
// gh login.
func readSOPSConfig(repo, ref string) ([]byte, error) {
	out, err := personGH("api", "repos/"+repo+"/contents/.sops.yaml?ref="+url.QueryEscape(ref), "--jq", ".content")
	if err != nil {
		return nil, err
	}
	return base64.StdEncoding.DecodeString(strings.ReplaceAll(out, "\n", ""))
}

// kustomizeControllerReady says whether a Ready kustomize-controller
// Deployment runs anywhere in the lab.
func kustomizeControllerReady(ctx context.Context) (bool, error) {
	k, err := labKube()
	if err != nil {
		return false, err
	}
	list, err := k.clientset.AppsV1().Deployments("").List(ctx, metav1.ListOptions{FieldSelector: "metadata.name=kustomize-controller"})
	if err != nil {
		return false, err
	}
	for _, d := range list.Items {
		if d.Status.ReadyReplicas > 0 {
			return true, nil
		}
	}
	return false, nil
}

// requestFluxReconcile annotates a Flux object for an immediate reconcile.
func requestFluxReconcile(ctx context.Context, gvr schema.GroupVersionResource, ns, name string) error {
	body := fmt.Appendf(nil, `{"metadata":{"annotations":{%q:%q}}}`, fluxReconcileAnnotation, time.Now().UTC().Format(time.RFC3339Nano))
	return patchObject(ctx, gvr, ns, name, types.MergePatchType, body)
}

// fluxSourceGVR is the operator's --flux-source-kind as a resource.
func fluxSourceGVR(kind string) (schema.GroupVersionResource, error) {
	switch kind {
	case "", defaultFluxSourceKind:
		return gitRepositoryGVR, nil
	case kindOCIRepository:
		return gvrFor(fluxOCIRepositoryResource)
	}
	return schema.GroupVersionResource{}, fmt.Errorf("unknown Flux source kind %q", kind)
}

// waitKustomizationApplied waits until the Kustomization is Ready on a
// revision that names commit (Flux writes <branch>@sha1:<commit>).
func waitKustomizationApplied(ctx context.Context, ns, name, commit string) (string, error) {
	var last string
	ok := waitFor(int(credentialsFluxWait/pollInterval), pollInterval, func() bool {
		obj, err := getObject(ctx, kustomizationGVR, ns, name)
		if err != nil {
			last = err.Error()
			return false
		}
		revision, _, _ := unstructured.NestedString(obj.Object, "status", "lastAppliedRevision")
		status, reason, message := releaseReady(obj)
		last = fmt.Sprintf("Ready=%q %s %s, lastAppliedRevision %q", status, reason, excerpt(message, 200), revision)
		return status == conditionTrue && strings.HasSuffix(revision, commit)
	})
	if !ok {
		return "", fmt.Errorf("the Kustomization %s/%s did not apply %s within %s (last: %s)", ns, name, commit, credentialsFluxWait, last)
	}
	return last, nil
}

// secretValueSum is the SHA-256 of a Secret's key as the API server holds
// it, read with the lab's admin client; the value stays in this function.
func secretValueSum(ctx context.Context, ns, name, key string) ([sha256.Size]byte, bool, error) {
	k, err := labKube()
	if err != nil {
		return [sha256.Size]byte{}, false, err
	}
	secret, err := k.clientset.CoreV1().Secrets(ns).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return [sha256.Size]byte{}, false, nil
	}
	if err != nil {
		return [sha256.Size]byte{}, false, err
	}
	value, ok := secret.Data[key]
	if !ok || len(value) == 0 {
		return [sha256.Size]byte{}, false, nil
	}
	return sha256.Sum256(value), true, nil
}

// shortSum is a hash as the report prints it.
func shortSum(sum [sha256.Size]byte) string {
	return fmt.Sprintf("sha256:%x", sum[:6])
}
