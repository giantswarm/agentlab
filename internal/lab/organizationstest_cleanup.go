package lab

import (
	"context"
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

	"github.com/giantswarm/agentlab/internal/config"

	ateapi "github.com/giantswarm/agentlab/internal/kagent/gen"
	apiv1alpha1 "github.com/giantswarm/agentlab/internal/kagent/gen/kagent/api/v1alpha1"
)

const (
	organizationCleanupPart    = "cleanup"
	organizationCleanupFixture = "agentlab-organizations-inventory"
	organizationCleanupWait    = 15 * time.Minute
	organizationSnapshotBody   = "agentlab organization cleanup control"
)

func organizationProofPart(part string) (string, error) {
	switch part {
	case "":
		return "", nil
	case organizationCleanupPart, "erasure":
		return organizationCleanupPart, nil
	case organizationCredentialsPart:
		return organizationCredentialsPart, nil
	default:
		return "", fmt.Errorf("unknown organizations proof part %q: use cleanup or credentials, or omit --part", part)
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
			note("cleanup failed: retaining proof organizations, sessions and installation inventory for diagnosis: %v", resultErr)
		}
	}()
	organizationGVR, err := gvrFor(organizationResource)
	if err != nil {
		return "", err
	}
	organization, err := getObject(ctx, organizationGVR, organizationsRootNamespace, p.names[0])
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
	if err := waitOrganizationOrphaned(ctx, p.names[0], false); err != nil {
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
	control, err := getObject(ctx, organizationGVR, organizationsRootNamespace, p.names[1])
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
	if err := waitOrganizationOrphaned(ctx, p.names[0], true); err != nil {
		return "", err
	}
	note("organization %s/%s is Orphaned=True after its inventory entry was removed; session %s belongs to %s", organizationsRootNamespace, p.names[0], session.GetId(), p.members[0].Email)
	if err := p.deleteProofOrganization(ctx, 0); err != nil {
		return "", err
	}
	if err := wait.PollUntilContextTimeout(ctx, pollInterval, time.Minute, true, func(ctx context.Context) (bool, error) {
		remaining, err := organizationActors(ctx, api, p.namespaces[0])
		return len(remaining) == 0, err
	}); err != nil {
		return "", fmt.Errorf("organization actors remain: %w", err)
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
	removedSession, readErr := p.apis[0].getSession(ctx, session.GetId())
	if readErr == nil && removedSession.GetState() != apiv1alpha1.RuntimeState_RUNTIME_STATE_DELETED {
		return "", fmt.Errorf("deleted organization retains session %s in state %s", session.GetId(), sessionState(removedSession))
	}
	if readErr != nil && status.Code(readErr) != codes.NotFound && status.Code(readErr) != codes.PermissionDenied {
		return "", readErr
	}
	_, resumeErr := p.apis[0].sessions.ResumeSession(p.apis[0].callCtx(ctx), &apiv1alpha1.ResumeSessionRequest{SessionId: session.GetId()})
	if code := status.Code(resumeErr); code != codes.NotFound && code != codes.FailedPrecondition && code != codes.PermissionDenied {
		return "", fmt.Errorf("deleted session remains runnable: ResumeSession returned %v", resumeErr)
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
	return fmt.Sprintf("member-owned READY session %s and %d actors removed; both organizations and namespaces gone; snapshot prefix %s empty; control %s survived the first cleanup and was deleted with its own organization", session.GetId(), len(actors), prefix, controlKey), nil
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

func (p *organizationsProof) deleteProofOrganization(ctx context.Context, index int) error {
	organizationGVR, err := gvrFor(organizationResource)
	if err != nil {
		return err
	}
	organization, err := getObject(ctx, organizationGVR, organizationsRootNamespace, p.names[index])
	if err != nil {
		return err
	}
	if organization.GetLabels()[organizationsTestLabel] != organizationsTestMark {
		return fmt.Errorf("refusing to delete organization %s without the proof ownership label", organization.GetName())
	}
	token, err := passwordGrant(p.cfg, config.KubernetesClientID, config.KubernetesClientSecret, p.admin.Email, p.admin.Password, loginScopes)
	if err != nil {
		return err
	}
	admin, _, err := personClients(token)
	if err != nil {
		return err
	}
	resource := admin.Resource(organizationGVR).Namespace(organizationsRootNamespace)
	if _, err := resource.Patch(ctx, organization.GetName(), types.MergePatchType, []byte(`{"metadata":{"annotations":{"kustomize.toolkit.fluxcd.io/prune":"enabled"}}}`), metav1.PatchOptions{}); err != nil {
		return err
	}
	uid := organization.GetUID()
	if err := resource.Delete(ctx, organization.GetName(), metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}}); err != nil {
		return err
	}
	last := ""
	err = wait.PollUntilContextTimeout(ctx, pollInterval, organizationCleanupWait, true, func(ctx context.Context) (bool, error) {
		current, err := getObject(ctx, organizationGVR, organizationsRootNamespace, organization.GetName())
		if apierrors.IsNotFound(err) {
			_, err = getObject(ctx, gvrNamespaces, "", p.namespaces[index])
			return apierrors.IsNotFound(err), ignoreNotFound(err)
		}
		if err != nil {
			return false, err
		}
		step, _, _ := unstructured.NestedString(current.Object, "status", "erasureStep")
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
