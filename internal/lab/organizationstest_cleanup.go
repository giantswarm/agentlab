package lab

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"

	"github.com/giantswarm/agentlab/internal/config"

	ateapi "github.com/giantswarm/agentlab/internal/kagent/gen"
	apiv1alpha1 "github.com/giantswarm/agentlab/internal/kagent/gen/kagent/api/v1alpha1"
)

const (
	organizationCleanupPart    = "cleanup"
	organizationCleanupFixture = "agentlab-organizations-inventory"
	organizationCleanupWait    = 15 * time.Minute
	// organizationActorsGoneWait bounds the wait for an erased
	// organization's actors to go, a transport error of the read included.
	organizationActorsGoneWait = time.Minute
	organizationSnapshotBody   = "agentlab organization cleanup control"
	// organizationDeleteIntentPolicy is the chart's ValidatingAdmissionPolicy
	// that holds an Organization's delete: an organization applied from git
	// (it has an Orphaned condition) is erased only once its delete pull
	// request merged (Orphaned=True, reason DeleteMerged).
	organizationDeleteIntentPolicy = "agent-platform-organization-delete-intent"
	// organizationDeletePullRequestAnnotation is what agent-manager writes in
	// mode commit on an Organization whose delete pull request it opened: the
	// pull request's URL. The operator gives an orphan the reason DeleteMerged
	// only with it.
	organizationDeletePullRequestAnnotation = "agent-platform.giantswarm.io/delete-pull-request"
	// organizationCleanupPullRequest is the proof's value of that annotation:
	// URL-shaped, as agent-manager writes it, and naming no real pull request.
	organizationCleanupPullRequest = "https://github.invalid/giantswarm/agentlab-organizations-test/pull/1"
	// fluxPruneAnnotation is Flux's marker of an intended prune, which the
	// delete-intent policy asks of every delete.
	fluxPruneAnnotation = "kustomize.toolkit.fluxcd.io/prune"
	// The operator's Orphaned condition and its reasons.
	orphanedCondition    = "Orphaned"
	orphanedInInventory  = "InInventory"
	orphanedMissingInGit = "MissingFromGit"
	orphanedDeleteMerged = "DeleteMerged"
)

func organizationProofPart(part string) (string, error) {
	switch part {
	case "":
		return "", nil
	case organizationCleanupPart, "erasure":
		return organizationCleanupPart, nil
	case organizationCredentialsPart:
		return organizationCredentialsPart, nil
	case organizationAdminsPart:
		return organizationAdminsPart, nil
	default:
		return "", fmt.Errorf("unknown organizations proof part %q: use cleanup, credentials or admins, or omit --part", part)
	}
}

func (p *organizationsProof) cleanupCheck() organizationCheck {
	return organizationCheck{surface: surfaceProfile, boundary: "cleanup of an Orphaned organization with a running session finishes", run: p.cleanupOrphanedOrganization}
}

func (p *organizationsProof) cleanupOrphanedOrganization() (detail string, resultErr error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*organizationCleanupWait)
	defer cancel()
	build, err := p.apis[0].version(ctx)
	if err != nil {
		return "", err
	}
	version, err := semver.NewVersion(build.GetKagentVersion())
	if err != nil || version.LessThan(semver.MustParse("1.31.1-rc.3")) {
		return "", fmt.Errorf("orphan cleanup needs kagent 1.31.1-rc.3 or newer with cross-creator Session deletion, got %q (%s)", build.GetKagentVersion(), build.GetGitCommit())
	}
	note("kagent %s (%s) carries cross-creator Session deletion", build.GetKagentVersion(), build.GetGitCommit())
	p.retainCleanupDiagnostics = true
	defer func() {
		if resultErr != nil {
			note("cleanup failed: retaining proof organizations, sessions and installation inventory for diagnosis; the next run removes them first: %v", resultErr)
		}
	}()
	organizationGVR, err := gvrFor(organizationResource)
	if err != nil {
		return "", err
	}
	organization, err := getObject(ctx, organizationGVR, "", p.names[0])
	if err != nil {
		return "", err
	}
	removeFixture, err := prepareOrganizationInventory(ctx, organization)
	if err != nil {
		return "", err
	}
	// The fixture and operator setting stay readable when cleanup fails.
	defer func() {
		if resultErr == nil {
			resultErr = removeFixture()
		}
	}()
	if err := waitOrganizationOrphaned(ctx, p.names[0], false, orphanedInInventory); err != nil {
		return "", err
	}

	api, err := dialAteAPI(ctx)
	if err != nil {
		return "", err
	}
	defer api.Close()
	session, err := p.apis[0].createSessionOf(ctx, &apiv1alpha1.ResourceReference{Namespace: p.namespaces[0], Name: organizationsTestAgent}, "organization-cleanup-"+string(organization.GetUID()))
	if err != nil {
		return "", err
	}
	p.sessions[0] = append(p.sessions[0], session.GetId())
	if session.GetState() == apiv1alpha1.RuntimeState_RUNTIME_STATE_SUSPENDED {
		session, err = p.apis[0].resumeSession(ctx, session.GetId())
		if err != nil {
			return "", err
		}
	}
	if session.GetState() != apiv1alpha1.RuntimeState_RUNTIME_STATE_READY {
		return "", fmt.Errorf("member session %s is %s, want READY", session.GetId(), sessionState(session))
	}
	actors, err := organizationActors(ctx, api, p.namespaces[0])
	if err != nil {
		return "", err
	}
	if len(actors) == 0 {
		return "", fmt.Errorf("READY member session %s has no actor in %s", session.GetId(), p.namespaces[0])
	}

	client, bucket, stop, err := organizationSnapshotClient(ctx)
	if err != nil {
		return "", err
	}
	defer stop()
	control, err := getObject(ctx, organizationGVR, "", p.names[1])
	if err != nil {
		return "", err
	}
	prefix := string(organization.GetUID()) + "/"
	controlKey := string(control.GetUID()) + "/agentlab-cleanup-control"
	for _, key := range []string{prefix + "agentlab-cleanup-snapshot", controlKey} {
		if _, err := client.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), Body: strings.NewReader(organizationSnapshotBody)}); err != nil {
			return "", fmt.Errorf("seed snapshot object %s: %w", key, err)
		}
	}
	if _, err := client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String(prefix + "agentlab-cleanup-snapshot")}); err != nil {
		return "", fmt.Errorf("seeded snapshot is not readable: %w", err)
	}
	if err := writeOrganizationInventory(ctx); err != nil {
		return "", err
	}
	if err := waitOrganizationOrphaned(ctx, p.names[0], true, orphanedMissingInGit); err != nil {
		return "", err
	}
	note("organization %s is Orphaned=True (%s) after its inventory entry was removed; session %s belongs to %s", p.names[0], orphanedMissingInGit, session.GetId(), p.members[0].Email)
	// The refusal first: Flux's prune marker alone does not erase an
	// organization whose file is gone from git without a delete pull request.
	refused, err := p.deleteRefusedWhileMissingFromGit(ctx, session.GetId())
	if err != nil {
		return "", err
	}
	p.add(organizationVerdict{surface: surfaceProfile, boundary: "the delete of an Orphaned organization missing from git, without a merged delete pull request, is refused", detail: refused})
	// Then the path of a merged delete pull request, as agent-manager takes
	// it in mode commit.
	if err := p.markDeletePullRequest(ctx, p.names[0]); err != nil {
		return "", err
	}
	if err := waitOrganizationOrphaned(ctx, p.names[0], true, orphanedDeleteMerged); err != nil {
		return "", err
	}
	note("organization %s is Orphaned=True (%s) with %s: %s", p.names[0], orphanedDeleteMerged, organizationDeletePullRequestAnnotation, organizationCleanupPullRequest)
	if err := p.deleteProofOrganization(ctx, 0); err != nil {
		return "", err
	}
	if err := wait.PollUntilContextTimeout(ctx, pollInterval, organizationActorsGoneWait, true, func(ctx context.Context) (bool, error) {
		var remaining []*ateapi.Actor
		err := endStateRead(ctx, "ListActors", p.namespaces[0], organizationActorsGoneWait, func() (err error) {
			remaining, err = organizationActors(ctx, api, p.namespaces[0])
			return err
		})
		return len(remaining) == 0, err
	}); err != nil {
		return "", fmt.Errorf("organization actors of %s not gone within %s: %w", p.namespaces[0], organizationActorsGoneWait, err)
	}
	if err := requireSnapshotCount(ctx, client, bucket, prefix, 0); err != nil {
		return "", err
	}
	got, err := client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(controlKey)})
	if err != nil {
		return "", fmt.Errorf("control object did not survive: %w", err)
	}
	body, readErr := io.ReadAll(io.LimitReader(got.Body, 1024))
	closeErr := got.Body.Close()
	if readErr != nil {
		return "", readErr
	}
	if closeErr != nil {
		return "", closeErr
	}
	if string(body) != organizationSnapshotBody {
		return "", fmt.Errorf("control object content changed")
	}
	if err := requireSessionErased(ctx, p.apis[0], session.GetId()); err != nil {
		return "", err
	}
	p.sessions[0] = nil
	if err := p.deleteProofOrganization(ctx, 1); err != nil {
		return "", err
	}
	if err := requireSnapshotCount(ctx, client, bucket, string(control.GetUID())+"/", 0); err != nil {
		return "", err
	}
	p.sessions[1] = nil
	p.retainCleanupDiagnostics = false
	return fmt.Sprintf("after its delete pull request merged (Orphaned=True, %s), member-owned READY session %s and %d actors removed; both organizations and namespaces gone; snapshot prefix %s empty; control %s survived the first cleanup and was deleted with its own organization", orphanedDeleteMerged, session.GetId(), len(actors), prefix, controlKey), nil
}

// endStateReadTimeout bounds the retries of an end-state read of the cleanup
// proof on a transport error: the kagent controller restarts with its
// database when the kubelet restarts that for a failed liveness probe, as on
// a loaded laptop. A variable so the tests wait in milliseconds.
var endStateReadTimeout = 3 * time.Minute

// The SessionService methods the cleanup proof's end-state reads call.
const (
	getSessionMethod    = "GetSession"
	resumeSessionMethod = "ResumeSession"
)

// transportError is a gRPC answer that says nothing of the end state: the
// connection to the server failed (Unavailable), as on its restart.
func transportError(err error) bool {
	return status.Code(err) == codes.Unavailable
}

// endStateRead runs read, a read that proves an end state of the cleanup
// proof, again on a transport error until timeout, noting the first one;
// every other answer returns at once for the caller to judge.
func endStateRead(ctx context.Context, method, id string, timeout time.Duration, read func() error) error {
	return untilSettled(ctx, method, id, timeout, transportError, read)
}

// requireSessionErased proves the session id erased with its organization:
// GetSession answers NotFound or PermissionDenied, or the session DELETED,
// and ResumeSession answers NotFound, FailedPrecondition or PermissionDenied.
// A transport error of either read is tried again (endStateRead); any other
// answer, and a transport error past the bound, fails the proof.
func requireSessionErased(ctx context.Context, api *kagentAPI, id string) error {
	var session *apiv1alpha1.Session
	readErr := endStateRead(ctx, getSessionMethod, id, endStateReadTimeout, func() error {
		response, err := api.sessions.GetSession(api.callCtx(ctx), &apiv1alpha1.GetSessionRequest{SessionId: id})
		session = response.GetSession()
		return err
	})
	switch code := status.Code(readErr); {
	case readErr == nil && session.GetState() != apiv1alpha1.RuntimeState_RUNTIME_STATE_DELETED:
		return fmt.Errorf("deleted organization retains session %s in state %s", id, sessionState(session))
	case readErr != nil && code != codes.NotFound && code != codes.PermissionDenied:
		return readErr
	}
	resumeErr := endStateRead(ctx, resumeSessionMethod, id, endStateReadTimeout, func() error {
		_, err := api.sessions.ResumeSession(api.callCtx(ctx), &apiv1alpha1.ResumeSessionRequest{SessionId: id})
		return err
	})
	switch code := status.Code(resumeErr); {
	case resumeErr == nil:
		return fmt.Errorf("deleted session %s remains runnable: ResumeSession succeeded", id)
	case code != codes.NotFound && code != codes.FailedPrecondition && code != codes.PermissionDenied:
		return fmt.Errorf("deleted session %s is not proven unrunnable: %w", id, resumeErr)
	}
	return nil
}

func organizationActors(ctx context.Context, api *ateAPI, namespace string) ([]*ateapi.Actor, error) {
	var actors []*ateapi.Actor
	pageToken := ""
	for range substrateMaxPages {
		response, err := api.ListActors(ctx, &ateapi.ListActorsRequest{Atespace: namespace, PageToken: pageToken})
		if err != nil {
			return nil, err
		}
		actors = append(actors, response.GetActors()...)
		pageToken = response.GetNextPageToken()
		if pageToken == "" {
			return actors, nil
		}
	}
	return nil, fmt.Errorf("actors of %s still paging after %d pages", namespace, substrateMaxPages)
}

func requireSnapshotCount(ctx context.Context, client *s3.Client, bucket, prefix string, expected int) error {
	pages := s3.NewListObjectsV2Paginator(client, &s3.ListObjectsV2Input{Bucket: aws.String(bucket), Prefix: aws.String(prefix)})
	count := 0
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return err
		}
		count += len(page.Contents)
	}
	if count != expected {
		return fmt.Errorf("snapshot prefix %s holds %d objects, want %d", prefix, count, expected)
	}
	return nil
}

// deleteProofOrganization erases the proof's organization index as the
// lab's admin and waits until it and its namespace are gone. It refuses an
// organization without the proof's ownership label.
func (p *organizationsProof) deleteProofOrganization(ctx context.Context, index int) error {
	organization, err := p.ownedOrganization(ctx, p.names[index])
	if err != nil {
		return err
	}
	resource, err := p.adminOrganizations()
	if err != nil {
		return err
	}
	return eraseOrganization(ctx, resource, organization, p.namespaces[index])
}

// ownedOrganization reads the organization name and refuses it without the
// proof's ownership label.
func (p *organizationsProof) ownedOrganization(ctx context.Context, name string) (*unstructured.Unstructured, error) {
	organizationGVR, err := gvrFor(organizationResource)
	if err != nil {
		return nil, err
	}
	organization, err := getObject(ctx, organizationGVR, "", name)
	if err != nil {
		return nil, err
	}
	if organization.GetLabels()[organizationsTestLabel] != organizationsTestMark {
		return nil, fmt.Errorf("refusing to delete organization %s without the proof ownership label %s=%s", name, organizationsTestLabel, organizationsTestMark)
	}
	return organization, nil
}

// adminOrganizations is the Organizations resource as the lab's admin, on
// the admin's own Dex token: the person whose delete the chart's admission
// judges.
func (p *organizationsProof) adminOrganizations() (dynamic.ResourceInterface, error) {
	organizationGVR, err := gvrFor(organizationResource)
	if err != nil {
		return nil, err
	}
	token, err := passwordGrant(p.cfg, config.KubernetesClientID, config.KubernetesClientSecret, p.admin.Email, p.admin.Password, loginScopes)
	if err != nil {
		return nil, err
	}
	admin, _, err := personClients(token)
	if err != nil {
		return nil, err
	}
	return admin.Resource(organizationGVR), nil
}

// deleteRefusedWhileMissingFromGit is the refusal verdict: the lab's admin
// deletes the Orphaned organization (reason MissingFromGit) with Flux's prune
// marker, the chart's delete-intent policy refuses it, and the organization,
// its namespace and the member's session are still there.
func (p *organizationsProof) deleteRefusedWhileMissingFromGit(ctx context.Context, sessionID string) (string, error) {
	organization, err := p.ownedOrganization(ctx, p.names[0])
	if err != nil {
		return "", err
	}
	resource, err := p.adminOrganizations()
	if err != nil {
		return "", err
	}
	uid := organization.GetUID()
	if _, err := resource.Patch(ctx, organization.GetName(), types.MergePatchType, pruneMarkerPatch(uid), metav1.PatchOptions{}); err != nil {
		return "", err
	}
	what := fmt.Sprintf("the lab admin's delete of organization %s (Orphaned=True, %s, %s: enabled)", organization.GetName(), orphanedMissingInGit, fluxPruneAnnotation)
	if err := deleteIntentRefusal(what, resource.Delete(ctx, organization.GetName(), metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}})); err != nil {
		return "", err
	}
	organizationGVR, err := gvrFor(organizationResource)
	if err != nil {
		return "", err
	}
	current, err := getObject(ctx, organizationGVR, "", organization.GetName())
	if err != nil {
		return "", fmt.Errorf("organization %s after the refused delete: %w", organization.GetName(), err)
	}
	if current.GetUID() != uid || current.GetDeletionTimestamp() != nil {
		return "", fmt.Errorf("organization %s is being deleted or was replaced after the refused delete", organization.GetName())
	}
	if _, err := getObject(ctx, gvrNamespaces, "", p.namespaces[0]); err != nil {
		return "", fmt.Errorf("namespace %s after the refused delete: %w", p.namespaces[0], err)
	}
	session, err := p.apis[0].getSession(ctx, sessionID)
	if err != nil {
		return "", fmt.Errorf("session %s after the refused delete: %w", sessionID, err)
	}
	if session.GetState() == apiv1alpha1.RuntimeState_RUNTIME_STATE_DELETED {
		return "", fmt.Errorf("session %s is DELETED after the refused delete", sessionID)
	}
	return fmt.Sprintf("%s refused by the ValidatingAdmissionPolicy %s; organization %s, namespace %s and session %s (%s) still there", what, organizationDeleteIntentPolicy, organization.GetName(), p.namespaces[0], sessionID, sessionState(session)), nil
}

// deleteIntentRefusal reads a delete's answer as the delete-intent policy's
// refusal of an organization applied from git whose delete pull request has
// not merged: nil for that refusal, an error naming what happened otherwise
// (the delete was admitted, or something else refused it, the same policy's
// check of the prune marker among them).
func deleteIntentRefusal(what string, err error) error {
	switch {
	case err == nil:
		return fmt.Errorf("%s was admitted; the ValidatingAdmissionPolicy %s refuses it until the delete pull request merged", what, organizationDeleteIntentPolicy)
	case (apierrors.IsForbidden(err) || apierrors.IsInvalid(err)) &&
		strings.Contains(err.Error(), validatingAdmissionPolicy+" '"+organizationDeleteIntentPolicy+"'") &&
		strings.Contains(err.Error(), orphanedDeleteMerged):
		return nil
	}
	return fmt.Errorf("%s failed, but not as the refusal of the ValidatingAdmissionPolicy %s for an organization without a merged delete pull request: %w", what, organizationDeleteIntentPolicy, err)
}

// markDeletePullRequest sets the delete pull request annotation on the
// organization name as the lab's admin, as agent-manager does in mode commit.
func (p *organizationsProof) markDeletePullRequest(ctx context.Context, name string) error {
	organization, err := p.ownedOrganization(ctx, name)
	if err != nil {
		return err
	}
	resource, err := p.adminOrganizations()
	if err != nil {
		return err
	}
	return patchDeletePullRequest(ctx, resource, name, organization.GetUID())
}

// patchDeletePullRequest sets the delete pull request annotation on the
// organization name with uid: the uid in the patch is the API server's
// precondition, so a replacement of that name is not written.
func patchDeletePullRequest(ctx context.Context, resource dynamic.ResourceInterface, name string, uid types.UID) error {
	patch, err := json.Marshal(map[string]any{crMetadata: map[string]any{"uid": uid, "annotations": map[string]string{organizationDeletePullRequestAnnotation: organizationCleanupPullRequest}}})
	if err != nil {
		return err
	}
	if _, err := resource.Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("annotating organization %s with %s: %w", name, organizationDeletePullRequestAnnotation, err)
	}
	return nil
}

// pruneMarkerPatch sets Flux's prune marker on the organization with uid,
// which the API server checks as a precondition.
func pruneMarkerPatch(uid types.UID) []byte {
	patch, _ := json.Marshal(map[string]any{crMetadata: map[string]any{"uid": uid, "annotations": map[string]string{fluxPruneAnnotation: "enabled"}}})
	return patch
}

// eraseOrganization deletes organization through resource with Flux's prune
// marker, unless its deletion already runs, and waits until it and its
// namespace are gone, noting each erasure step.
func eraseOrganization(ctx context.Context, resource dynamic.ResourceInterface, organization *unstructured.Unstructured, namespace string) error {
	organizationGVR, err := gvrFor(organizationResource)
	if err != nil {
		return err
	}
	if organization.GetDeletionTimestamp() == nil {
		uid := organization.GetUID()
		if _, err := resource.Patch(ctx, organization.GetName(), types.MergePatchType, pruneMarkerPatch(uid), metav1.PatchOptions{}); err != nil {
			return err
		}
		if err := resource.Delete(ctx, organization.GetName(), metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}}); err != nil {
			return err
		}
	}
	last := ""
	err = wait.PollUntilContextTimeout(ctx, pollInterval, organizationCleanupWait, true, func(ctx context.Context) (bool, error) {
		current, err := getObject(ctx, organizationGVR, "", organization.GetName())
		if apierrors.IsNotFound(err) {
			if namespace == "" {
				return true, nil
			}
			_, err = getObject(ctx, gvrNamespaces, "", namespace)
			return apierrors.IsNotFound(err), ignoreNotFound(err)
		}
		if err != nil {
			return false, err
		}
		state, err := organizationStateFrom(current)
		if err != nil {
			return false, err
		}
		step := state.erasureStep
		if step != last {
			note("organization %s cleanup: %s", organization.GetName(), step)
			last = step
		}
		return false, nil
	})
	if err != nil {
		return fmt.Errorf("cleaning up organization %s at step %s: %w", organization.GetName(), last, err)
	}
	return nil
}

func ignoreNotFound(err error) error {
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

func organizationSnapshotClient(ctx context.Context) (*s3.Client, string, func(), error) {
	k, err := labKube()
	if err != nil {
		return nil, "", nil, err
	}
	deployment, err := k.clientset.AppsV1().Deployments(platformNamespace).Get(ctx, "agent-platform-operator", metav1.GetOptions{})
	if err != nil {
		return nil, "", nil, err
	}
	container := deployment.Spec.Template.Spec.Containers[0]
	endpoint, bucket, pathStyle := "", "", false
	for _, argument := range container.Args {
		if value, ok := strings.CutPrefix(argument, "--snapshot-endpoint="); ok {
			endpoint = value
		}
		if value, ok := strings.CutPrefix(argument, "--snapshot-bucket="); ok {
			bucket = value
		}
		if argument == "--snapshot-use-path-style" || argument == "--snapshot-use-path-style=true" {
			pathStyle = true
		}
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "http" || bucket == "" || !pathStyle {
		return nil, "", nil, fmt.Errorf("cleanup proof needs the operator's in-cluster HTTP path-style snapshot endpoint and bucket")
	}
	host := strings.Split(parsed.Hostname(), ".")
	if len(host) < 3 || host[2] != "svc" {
		return nil, "", nil, fmt.Errorf("snapshot endpoint must name an in-cluster Service")
	}
	service, err := k.clientset.CoreV1().Services(host[1]).Get(ctx, host[0], metav1.GetOptions{})
	if err != nil {
		return nil, "", nil, err
	}
	pods, err := k.targetPods(ctx, host[1], metav1.FormatLabelSelector(&metav1.LabelSelector{MatchLabels: service.Spec.Selector}))
	if err != nil {
		return nil, "", nil, err
	}
	podIndex := slices.IndexFunc(pods, func(pod corev1.Pod) bool { return podReady(&pod) })
	if podIndex < 0 {
		return nil, "", nil, fmt.Errorf("snapshot store has no Ready pod")
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil {
		return nil, "", nil, err
	}
	for _, servicePort := range service.Spec.Ports {
		if int(servicePort.Port) == port {
			port = int(servicePort.TargetPort.IntVal)
			break
		}
	}
	forwarded, stop, err := portForwardPod(ctx, host[1], pods[podIndex].Name, port)
	if err != nil {
		return nil, "", nil, err
	}
	values := map[string]string{}
	for _, variable := range container.Env {
		if variable.Name != "AWS_ACCESS_KEY_ID" && variable.Name != "AWS_SECRET_ACCESS_KEY" && variable.Name != "AWS_SESSION_TOKEN" && variable.Name != "AWS_REGION" {
			continue
		}
		if variable.ValueFrom != nil && variable.ValueFrom.SecretKeyRef != nil {
			reference := variable.ValueFrom.SecretKeyRef
			secret, err := k.clientset.CoreV1().Secrets(platformNamespace).Get(ctx, reference.Name, metav1.GetOptions{})
			if err != nil {
				stop()
				return nil, "", nil, fmt.Errorf("snapshot credential Secret %s: %w", reference.Name, err)
			}
			values[variable.Name] = string(secret.Data[reference.Key])
		} else {
			values[variable.Name] = variable.Value
		}
	}
	if values["AWS_ACCESS_KEY_ID"] == "" || values["AWS_SECRET_ACCESS_KEY"] == "" {
		stop()
		return nil, "", nil, fmt.Errorf("snapshot operator needs credential Secret references")
	}
	return s3.New(s3.Options{BaseEndpoint: aws.String(fmt.Sprintf("http://127.0.0.1:%d", forwarded)), UsePathStyle: true, Region: values["AWS_REGION"], Credentials: credentials.NewStaticCredentialsProvider(values["AWS_ACCESS_KEY_ID"], values["AWS_SECRET_ACCESS_KEY"], values["AWS_SESSION_TOKEN"])}), bucket, stop, nil
}
