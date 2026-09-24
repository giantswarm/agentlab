package lab

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/klaus-gateway/pkg/auth/musterlink"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/giantswarm/agentlab/internal/config"
)

// The component half of the Swarmgeist proof: the meta chart's klaus-gateway
// (platform.klausGateway, klausgateway.go) with its OBO link store in a
// Kubernetes Secret — the store the fleet runs since klaus-gateway 1.3.0,
// which replaced the ReadWriteOnce claim that turned every node loss into a
// four-minute gateway outage (a replacement pod waiting on the volume the
// dead node still held). What the lab proves about it:
//
//   - the render: the Role grants get/update/patch on exactly the link
//     Secret (resourceNames), the RoleBinding binds the gateway's
//     ServiceAccount, the Secret is the chart's with resource-policy keep,
//     the Deployment reads the Secret store, mounts no store volume and
//     keeps the RollingUpdate strategy the claim used to force to Recreate;
//   - the store: two links written through the gateway's own store package
//     (github.com/giantswarm/klaus-gateway/pkg/auth/musterlink) with the
//     lab's store-key, so they are sealed byte for byte as the gateway seals
//     them; the pod is deleted — the node-loss shape, not a rolling restart —
//     and its replacement is Ready and reports the same link count in its
//     `obo link store ready` record within seconds; both links read back
//     through the package unchanged; the proof's records removed, and a
//     leftover of an aborted run removed first;
//   - the in-cluster a2a leg: through a port-forward to the replacement pod,
//     one signed Slack mention by the person whose link the store step
//     wrote (the lab user's id_token as its cached token) is answered in the
//     proof's fake thread — the component's Slack Web API is the proof's
//     fake through a selector-less Service the proof points at this host,
//     and it talks to the controller over the in-cluster plaintext h2c
//     target, which the host-mode gateway never touches.
//
// Not provable here, and stated in docs/klaus-gateway.md: a real OBO sign-in
// and a link's refresh at muster. The linker's callback checks the muster
// identity's e-mail against the Slack workspace's, and muster refuses a CIMD
// client_id on a private-IP hostname, so no sign-in completes and no muster
// refresh token exists for the gateway to spend.

// The Slack user ids of the proof's link records (the keys of the link
// Secret; the values are sealed) carry slackUserPrefix, so a leftover of an
// aborted run is recognisable by its key alone. legacyLinkPrefix is the
// prefix earlier agentlab versions filed them under.
const legacyLinkPrefix = "agentlab-klaus-gateway-test-"

// klausGatewayReplaceWait bounds the pod's replacement after the deletion: a
// new pod scheduled, the image already on the node, the store read at start,
// the readiness probe (5 s initial delay). The claim the store left needed
// minutes when the node was gone; the Secret needs none of it.
const klausGatewayReplaceWait = 60 * time.Second

// The gateway's records and the Deployment's shape the proof reads.
const (
	// storeReadyRecord is logged once the Secret store is read at start;
	// its `links` field is the count.
	storeReadyRecord = `"msg":"obo link store ready"`
	oboStoreEnv      = "KLAUS_GATEWAY_OBO_STORE"
	// oboStoreSecretBackend is the store's name in the env and the record.
	oboStoreSecretBackend = "secret"
	oboStoreSecret        = "KLAUS_GATEWAY_OBO_STORE_SECRET"
	oboStoreVolume        = "obo-store"
	resourcePolicy        = "helm.sh/resource-policy"
	// gatewayHTTPPort is the port the component serves its channels on.
	gatewayHTTPPort = 8080
)

// componentOutcome is what the component half found, for the summary.
type componentOutcome struct {
	pod, replacement string
	version          string
	baseline, links  int
	elapsed          time.Duration
	answer           string
}

// klausGatewayComponentProof runs the component half (see the file header)
// as the signed-in person: the render, the store across a pod loss, one Slack
// turn through the replacement pod on the proof's fake Web API, served to the
// pod by the Service the host-mode half pointed at it (slackUser is the
// person the turn is sent as). The fixtures of the host-mode half are in
// place; nothing here is left behind but what was there before.
func klausGatewayComponentProof(token string, identity linkedIdentity, user *config.User, fake slackWorkspace, slackUser string) (*componentOutcome, error) {
	ctx := context.Background()
	k, err := labKube()
	if err != nil {
		return nil, err
	}
	out := &componentOutcome{}

	step("6. The component: Deployment %s runs the OBO link store in Secret %s — the Role scoped to that Secret, no store volume, RollingUpdate — and calls the Slack Web API at %s", klausGatewayComponent, klausGatewayLinksSecret, klausGatewaySlackAPIBase)
	deploy, err := k.clientset.AppsV1().Deployments(platformNamespace).Get(ctx, klausGatewayComponent, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w (platform.klausGateway is on; did `agentlab platform` install the component?)", describe(gvrDeployments, platformNamespace, klausGatewayComponent), err)
	}
	if err := assertStoreDeployment(deploy); err != nil {
		return nil, err
	}
	if err := assertFakeSlackAPI(deploy); err != nil {
		return nil, err
	}
	role, err := k.clientset.RbacV1().Roles(platformNamespace).Get(ctx, klausGatewayLinksSecret, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("reading Role %s/%s: %w (the chart renders it with obo.store: secret)", platformNamespace, klausGatewayLinksSecret, err)
	}
	if err := assertLinksRole(role); err != nil {
		return nil, err
	}
	binding, err := k.clientset.RbacV1().RoleBindings(platformNamespace).Get(ctx, klausGatewayLinksSecret, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("reading RoleBinding %s/%s: %w", platformNamespace, klausGatewayLinksSecret, err)
	}
	if err := assertLinksRoleBinding(binding, deploy.Spec.Template.Spec.ServiceAccountName); err != nil {
		return nil, err
	}
	links, err := k.clientset.CoreV1().Secrets(platformNamespace).Get(ctx, klausGatewayLinksSecret, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("reading the link Secret %s/%s: %w", platformNamespace, klausGatewayLinksSecret, err)
	}
	if got := links.Annotations[resourcePolicy]; got != "keep" {
		return nil, fmt.Errorf("the link Secret %s carries %s=%q, want keep (the entries are people's sign-ins, not chart state)", klausGatewayLinksSecret, resourcePolicy, got)
	}
	pods, err := deploymentPods(ctx, k, deploy)
	if err != nil {
		return nil, err
	}
	out.pod = pods[0].Name
	note("Deployment %s (%s): %s=secret, no %s volume, strategy %s; Role %s: %s; RoleBinding to ServiceAccount %s; Secret %s kept, %d entries",
		klausGatewayComponent, deploy.Spec.Template.Spec.Containers[0].Image, oboStoreEnv, oboStoreVolume, strategyName(deploy),
		klausGatewayLinksSecret, ruleLine(role.Rules[0]), deploy.Spec.Template.Spec.ServiceAccountName, klausGatewayLinksSecret, len(links.Data))

	step("7. The link store: two links sealed and written through pkg/auth/musterlink with the lab's store-key (Secret %s) — %s's with the id_token as its cached token — a leftover of an aborted run removed first", klausGatewayOBOKeys, slackUser)
	key, err := secretDataKey(ctx, platformNamespace, klausGatewayOBOKeys, oboStoreKeyKey)
	if err != nil {
		return nil, err
	}
	var storeLog bytes.Buffer
	store, err := musterlink.NewSecretStore(k.clientset, key, musterlink.SecretStoreOptions{Namespace: platformNamespace, Name: klausGatewayLinksSecret},
		slog.New(slog.NewTextHandler(&storeLog, nil)))
	if err != nil {
		return nil, fmt.Errorf("opening the link store through the gateway's package: %w", err)
	}
	removed, err := removeProofLinks(store, links.Data)
	if err != nil {
		return nil, err
	}
	if removed > 0 {
		note("removed %d leftover record(s) of an earlier run", removed)
	}
	out.baseline, err = store.Check()
	if err != nil {
		return nil, fmt.Errorf("the link store: %w", err)
	}
	seeded := proofLinks(identity.link(user.Email, token), slackUser)
	for _, id := range slices.Sorted(maps.Keys(seeded)) {
		if err := store.Put(id, seeded[id]); err != nil {
			return nil, fmt.Errorf("writing link %s through the package: %w", id, err)
		}
	}
	defer func() {
		// Best effort on the error paths: the success path removes the records
		// itself and verifies the count.
		for id := range seeded {
			_ = store.Delete(id)
		}
	}()
	if count, err := store.Check(); err != nil {
		return nil, fmt.Errorf("the link store after the writes: %w", err)
	} else if count != out.baseline+len(seeded) {
		return nil, fmt.Errorf("the link Secret holds %d entries after %d writes onto %d, want %d; the store said: %s", count, len(seeded), out.baseline, out.baseline+len(seeded), excerpt(storeLog.String(), 400))
	}
	if err := readBackLinks(store, seeded); err != nil {
		return nil, err
	}
	note("%d entries in %s before; %d written and read back through the package (Sub, Email, RefreshToken, LinkedAt, the cached id_token and its expiry unchanged)", out.baseline, klausGatewayLinksSecret, len(seeded))

	step("8. Pod loss: pod %s is deleted; its replacement is Ready and reads the same %d links within %s", out.pod, out.baseline+len(seeded), klausGatewayReplaceWait)
	old := map[string]bool{}
	for _, p := range pods {
		old[p.Name] = true
		if err := k.clientset.CoreV1().Pods(platformNamespace).Delete(ctx, p.Name, metav1.DeleteOptions{}); err != nil {
			return nil, fmt.Errorf("deleting pod %s/%s: %w", platformNamespace, p.Name, err)
		}
	}
	start := time.Now()
	out.replacement, err = waitReplacementReady(ctx, k, deploy, old, klausGatewayReplaceWait)
	if err != nil {
		return nil, err
	}
	out.elapsed = time.Since(start).Round(100 * time.Millisecond)
	logs, err := waitStoreReady(ctx, out.replacement)
	if err != nil {
		return nil, err
	}
	out.version = gatewayVersion(logs)
	out.links, err = storeReadyLinks(logs)
	if err != nil {
		return nil, err
	}
	if want := out.baseline + len(seeded); out.links != want {
		return nil, fmt.Errorf("the replacement pod %s read %d links from %s, want %d: the store did not carry the records across the pod loss", out.replacement, out.links, klausGatewayLinksSecret, want)
	}
	if err := readBackLinks(store, seeded); err != nil {
		return nil, fmt.Errorf("after the pod loss: %w", err)
	}
	note("pod %s Ready %s after the deletion; %s read %d links from %s at start; both records still read back unchanged", out.replacement, out.elapsed, out.version, out.links, klausGatewayLinksSecret)

	step("9. One Slack turn through the component: a port-forward to pod %s, a signed mention as %s answered in the fake thread over the in-cluster target %s", out.replacement, slackUser, klausGatewayInClusterTarget)
	local, stopForward, err := portForwardPod(ctx, platformNamespace, out.replacement, gatewayHTTPPort)
	if err != nil {
		return nil, err
	}
	defer stopForward()
	signing, err := secretDataKey(ctx, platformNamespace, klausGatewaySlackPlaceholder, slackSigningSecretKey)
	if err != nil {
		return nil, err
	}
	channel := "CAGENTLAB" + strings.ToUpper(randomSuffix()) + "K"
	pod := out.replacement
	p := &slackProof{
		driver: newSlackDriver(fmt.Sprintf("http://127.0.0.1:%d", local), string(signing), fake, channel),
		fake:   fake, channel: channel,
		logs: func() (string, error) { return podLogs(ctx, platformNamespace, "pod/"+pod, klausGatewayComponent, 0) },
	}
	thread := &slackThread{user: slackUser}
	turn, err := p.firstTurn(thread, klausGatewayWordPrompt)
	if err != nil {
		return nil, fmt.Errorf("the turn through the component: %w", err)
	}
	if err := assertSlackTurnSaid(turn, klausGatewayWord); err != nil {
		return nil, fmt.Errorf("the turn through the component: %w", err)
	}
	if _, err := p.dispatch(thread, identity.subject, user.Email); err != nil {
		return nil, fmt.Errorf("the turn through the component: %w", err)
	}
	out.answer = turn.answer
	note("answered %q in thread %s as %q; turn_dispatch under %s", excerpt(turn.answer, 60), thread.ts, turn.stream.Username, user.Email)

	for _, id := range slices.Sorted(maps.Keys(seeded)) {
		if err := store.Delete(id); err != nil {
			return nil, fmt.Errorf("removing link %s through the package: %w", id, err)
		}
	}
	if count, err := store.Check(); err != nil {
		return nil, fmt.Errorf("the link store after the cleanup: %w", err)
	} else if count != out.baseline {
		return nil, fmt.Errorf("the link Secret holds %d entries after the proof's records were removed, want the %d it had before; the store said: %s", count, out.baseline, excerpt(storeLog.String(), 400))
	}
	return out, nil
}

// assertStoreDeployment checks the Deployment runs the Secret link store: the
// store env names it and the link Secret, no store volume is mounted, and
// the strategy is the default RollingUpdate (a mounted ReadWriteOnce claim
// is what forced Recreate).
func assertStoreDeployment(d *appsv1.Deployment) error {
	if len(d.Spec.Template.Spec.Containers) == 0 {
		return fmt.Errorf("the Deployment %s has no container", d.Name)
	}
	var container *corev1.Container
	for i := range d.Spec.Template.Spec.Containers {
		if d.Spec.Template.Spec.Containers[i].Name == klausGatewayComponent {
			container = &d.Spec.Template.Spec.Containers[i]
		}
	}
	if container == nil {
		return fmt.Errorf("the Deployment %s has no container %s", d.Name, klausGatewayComponent)
	}
	env := map[string]string{}
	for _, e := range container.Env {
		env[e.Name] = e.Value
	}
	if env[oboStoreEnv] != oboStoreSecretBackend {
		return fmt.Errorf("the Deployment %s runs %s=%q, want secret (klausGateway.obo.store: secret in the lab's values)", d.Name, oboStoreEnv, env[oboStoreEnv])
	}
	if env[oboStoreSecret] != klausGatewayLinksSecret {
		return fmt.Errorf("the Deployment %s names %s=%q as the link Secret, want %s", d.Name, oboStoreSecret, env[oboStoreSecret], klausGatewayLinksSecret)
	}
	for _, v := range d.Spec.Template.Spec.Volumes {
		if v.Name == oboStoreVolume {
			return fmt.Errorf("the Deployment %s still mounts a %s volume: the Secret store needs no volume, and a claim brings the Multi-Attach wait back", d.Name, oboStoreVolume)
		}
	}
	if d.Spec.Strategy.Type == appsv1.RecreateDeploymentStrategyType {
		return fmt.Errorf("the Deployment %s uses the Recreate strategy: only a mounted ReadWriteOnce claim needs it", d.Name)
	}
	return nil
}

// assertFakeSlackAPI checks the lab's postRenderer points the component's
// Slack Web API at the Service in front of the proof's fake — a lab rendered
// by an agentlab without it answers on slack.com, where the placeholder
// credentials fail.
func assertFakeSlackAPI(d *appsv1.Deployment) error {
	for _, c := range d.Spec.Template.Spec.Containers {
		if c.Name != klausGatewayComponent {
			continue
		}
		for _, e := range c.Env {
			if e.Name == slackAPIBaseEnv && e.Value == klausGatewaySlackAPIBase {
				return nil
			}
		}
	}
	return fmt.Errorf("the Deployment %s does not set %s=%s: run `agentlab platform` to render the lab's patch", d.Name, slackAPIBaseEnv, klausGatewaySlackAPIBase)
}

// strategyName words the Deployment's strategy.
func strategyName(d *appsv1.Deployment) string {
	if d.Spec.Strategy.Type == "" {
		return string(appsv1.RollingUpdateDeploymentStrategyType)
	}
	return string(d.Spec.Strategy.Type)
}

// linkSecretVerbs is what the chart grants on the link Secret: reading and
// updating, never creating (resourceNames cannot scope create, so the chart
// renders the Secret itself) and never deleting.
var linkSecretVerbs = []string{linkVerbGet, linkVerbUpdate, linkVerbPatch}

// The RBAC verbs, named once.
const (
	linkVerbGet    = "get"
	linkVerbUpdate = "update"
	linkVerbPatch  = "patch"
)

// assertLinksRole checks the Role grants exactly the link Secret's verbs on
// exactly that Secret and nothing else.
func assertLinksRole(role *rbacv1.Role) error {
	if len(role.Rules) != 1 {
		return fmt.Errorf("the Role %s has %d rules, want the one on the link Secret", role.Name, len(role.Rules))
	}
	rule := role.Rules[0]
	if !slices.Equal(rule.APIGroups, []string{""}) || !slices.Equal(rule.Resources, []string{"secrets"}) {
		return fmt.Errorf("the Role %s rule is on %v %v, want the core group's secrets", role.Name, rule.APIGroups, rule.Resources)
	}
	if !slices.Equal(rule.ResourceNames, []string{klausGatewayLinksSecret}) {
		return fmt.Errorf("the Role %s rule has resourceNames %v, want exactly [%s]: the gateway may touch no other Secret", role.Name, rule.ResourceNames, klausGatewayLinksSecret)
	}
	verbs := slices.Clone(rule.Verbs)
	slices.Sort(verbs)
	want := slices.Clone(linkSecretVerbs)
	slices.Sort(want)
	if !slices.Equal(verbs, want) {
		return fmt.Errorf("the Role %s grants %v on the link Secret, want exactly %v (no create, no delete, no list)", role.Name, rule.Verbs, linkSecretVerbs)
	}
	return nil
}

// ruleLine words a Role rule.
func ruleLine(rule rbacv1.PolicyRule) string {
	return fmt.Sprintf("%s on secrets %v", strings.Join(rule.Verbs, "/"), rule.ResourceNames)
}

// assertLinksRoleBinding checks the RoleBinding binds the gateway's
// ServiceAccount to the link Role.
func assertLinksRoleBinding(rb *rbacv1.RoleBinding, serviceAccount string) error {
	if rb.RoleRef.Kind != "Role" || rb.RoleRef.Name != klausGatewayLinksSecret {
		return fmt.Errorf("the RoleBinding %s refers to %s %s, want Role %s", rb.Name, rb.RoleRef.Kind, rb.RoleRef.Name, klausGatewayLinksSecret)
	}
	if serviceAccount == "" {
		serviceAccount = "default"
	}
	for _, s := range rb.Subjects {
		if s.Kind == rbacv1.ServiceAccountKind && s.Name == serviceAccount && (s.Namespace == "" || s.Namespace == rb.Namespace) {
			return nil
		}
	}
	return fmt.Errorf("the RoleBinding %s binds %v, want the gateway's ServiceAccount %s", rb.Name, rb.Subjects, serviceAccount)
}

// proofLinks are the two records the proof seeds: the person's link, filed
// under the Slack user the in-cluster turn is sent as, and a second lab
// user's whose refresh token is plainly a fake. Both ids carry
// slackUserPrefix.
func proofLinks(person *musterlink.Link, slackUser string) map[string]*musterlink.Link {
	now := time.Now().UTC().Truncate(time.Second)
	return map[string]*musterlink.Link{
		slackUser:       person,
		slackUser + "D": {Sub: "agentlab:dev", Email: "dev@lab.local", RefreshToken: linkRefreshMarker + "-dev", LinkedAt: now},
	}
}

// removeProofLinks deletes the records of earlier runs — the Secret's keys
// with the proof's prefixes — and reports how many.
func removeProofLinks(store *musterlink.SecretStore, data map[string][]byte) (int, error) {
	removed := 0
	for id := range data {
		if strings.HasPrefix(id, slackUserPrefix) || strings.HasPrefix(id, legacyLinkPrefix) {
			if err := store.Delete(id); err != nil {
				return removed, fmt.Errorf("removing leftover link %s: %w", id, err)
			}
			removed++
		}
	}
	return removed, nil
}

// readBackLinks reads every seeded record through the package and checks it
// decrypts to what was written.
func readBackLinks(store musterlink.Store, seeded map[string]*musterlink.Link) error {
	for _, id := range slices.Sorted(maps.Keys(seeded)) {
		got, err := store.Get(id)
		if errors.Is(err, musterlink.ErrNotLinked) {
			return fmt.Errorf("link %s is not in the store after it was written", id)
		}
		if err != nil {
			return fmt.Errorf("reading link %s back: %w", id, err)
		}
		if !sameLink(got, seeded[id]) {
			return fmt.Errorf("link %s read back differently: got sub=%s email=%s linked=%s expiry=%s, wrote sub=%s email=%s linked=%s expiry=%s", id,
				got.Sub, got.Email, got.LinkedAt.Format(time.RFC3339), got.Expiry.Format(time.RFC3339),
				seeded[id].Sub, seeded[id].Email, seeded[id].LinkedAt.Format(time.RFC3339), seeded[id].Expiry.Format(time.RFC3339))
		}
	}
	return nil
}

// sameLink compares what the proof writes and reads.
func sameLink(a, b *musterlink.Link) bool {
	return a.Sub == b.Sub && a.Email == b.Email && a.RefreshToken == b.RefreshToken && a.LinkedAt.Equal(b.LinkedAt) &&
		a.IDToken == b.IDToken && a.Expiry.Equal(b.Expiry)
}

// deploymentPods lists the Deployment's pods through its selector.
func deploymentPods(ctx context.Context, k *kubeClients, d *appsv1.Deployment) ([]corev1.Pod, error) {
	sel, err := metav1.LabelSelectorAsSelector(d.Spec.Selector)
	if err != nil {
		return nil, fmt.Errorf("%s has an unusable selector: %w", describe(gvrDeployments, d.Namespace, d.Name), err)
	}
	list, err := k.clientset.CoreV1().Pods(d.Namespace).List(ctx, metav1.ListOptions{LabelSelector: sel.String()})
	if err != nil {
		return nil, fmt.Errorf("listing the pods of %s: %w", describe(gvrDeployments, d.Namespace, d.Name), err)
	}
	if len(list.Items) == 0 {
		return nil, fmt.Errorf("%s has no pods", describe(gvrDeployments, d.Namespace, d.Name))
	}
	return list.Items, nil
}

// waitReplacementReady polls the Deployment's pods until one that was not
// among the deleted is Ready, and returns its name; the deadline reports what
// the pods looked like last.
func waitReplacementReady(ctx context.Context, k *kubeClients, d *appsv1.Deployment, old map[string]bool, timeout time.Duration) (string, error) {
	var replacement string
	var last []string
	err := wait.PollUntilContextTimeout(ctx, 500*time.Millisecond, timeout, true, func(ctx context.Context) (bool, error) {
		pods, err := deploymentPods(ctx, k, d)
		if err != nil {
			return false, nil
		}
		last = last[:0]
		for i := range pods {
			if old[pods[i].Name] {
				continue
			}
			last = append(last, pods[i].Name+" "+podStateSummary(&pods[i]))
			if podReady(&pods[i]) {
				replacement = pods[i].Name
				return true, nil
			}
		}
		return false, nil
	})
	if wait.Interrupted(err) {
		return "", fmt.Errorf("no replacement pod of %s was Ready within %s of the deletion (%s): the Secret store must need no volume to wait for", describe(gvrDeployments, d.Namespace, d.Name), timeout, strings.Join(last, "; "))
	}
	if err != nil {
		return "", err
	}
	return replacement, nil
}

// podReady reports a Running pod whose Ready condition holds.
func podReady(pod *corev1.Pod) bool {
	if pod.DeletionTimestamp != nil || pod.Status.Phase != corev1.PodRunning {
		return false
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// waitStoreReady reads the pod's log until the store-ready record is in it
// (the record is written before the readiness probe passes; the log stream
// may lag a moment) and returns the log.
func waitStoreReady(ctx context.Context, pod string) (string, error) {
	var logs string
	if !waitFor(10, time.Second, func() bool {
		var err error
		logs, err = podLogs(ctx, platformNamespace, "pod/"+pod, klausGatewayComponent, 0)
		return err == nil && strings.Contains(logs, storeReadyRecord)
	}) {
		return "", fmt.Errorf("pod %s/%s is Ready but its log carries no %s record; its log ends:\n%s", platformNamespace, pod, storeReadyRecord, tailLines(logs, 8))
	}
	return logs, nil
}

// storeReadyLinks is the `links` count of the last store-ready record in the
// gateway's log.
func storeReadyLinks(logs string) (int, error) {
	lines := strings.Split(logs, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if !strings.Contains(lines[i], storeReadyRecord) {
			continue
		}
		var rec struct {
			Links   *int   `json:"links"`
			Backend string `json:"backend"`
			Secret  string `json:"secret"`
		}
		start := strings.IndexByte(lines[i], '{')
		if start < 0 || json.Unmarshal([]byte(lines[i][start:]), &rec) != nil || rec.Links == nil {
			return 0, fmt.Errorf("the store-ready record carries no links count: %s", excerpt(lines[i], 300))
		}
		if rec.Backend != "" && rec.Backend != oboStoreSecretBackend {
			return 0, fmt.Errorf("the gateway runs the %s store, not the Secret store: %s", rec.Backend, excerpt(lines[i], 300))
		}
		return *rec.Links, nil
	}
	return 0, fmt.Errorf("no %s record in the gateway's log", storeReadyRecord)
}
