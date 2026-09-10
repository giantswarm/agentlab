package lab

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	clienttesting "k8s.io/client-go/testing"
)

// resourceNodes is the cluster-scoped resource the admin assertions ask about.
const resourceNodes = "nodes"

// TestCanIQuestion: the assertions are written in kubectl's `auth can-i`
// argument shape, and each maps to exactly one SelfSubjectAccessReview — no
// namespace flag asks in the kubeconfig's namespace (default), -n in that
// namespace, --all-namespaces / -A in every namespace; anything else is
// refused rather than silently ignored.
func TestCanIQuestion(t *testing.T) {
	for _, tc := range []struct {
		args               []string
		verb, resource, ns string
	}{
		{[]string{verbGet, resourceNodes}, verbGet, resourceNodes, defaultNamespace},
		{[]string{verbCreate, gvrDeployments.Resource, "-n", "kube-public"}, verbCreate, gvrDeployments.Resource, "kube-public"},
		{[]string{verbCreate, gvrDeployments.Resource, "--namespace", testNS}, verbCreate, gvrDeployments.Resource, testNS},
		{[]string{verbList, gvrPods.Resource, "--all-namespaces"}, verbList, gvrPods.Resource, ""},
		{[]string{verbList, gvrPods.Resource, "-A"}, verbList, gvrPods.Resource, ""},
	} {
		verb, resource, ns, err := canIQuestion(tc.args)
		if err != nil || verb != tc.verb || resource != tc.resource || ns != tc.ns {
			t.Errorf("canIQuestion(%v) = %q %q %q %v, want %q %q %q", tc.args, verb, resource, ns, err, tc.verb, tc.resource, tc.ns)
		}
	}
	for _, bad := range [][]string{{verbGet}, {verbGet, gvrPods.Resource, "-n"}, {verbGet, gvrPods.Resource, "--as=someone"}, nil} {
		if _, _, _, err := canIQuestion(bad); err == nil {
			t.Errorf("canIQuestion(%v) must be refused", bad)
		}
	}
}

// TestCanIAnswer: the answer is the CLI's "yes"/"no" as the token's identity,
// the namespace of each question as canIQuestion maps it; a question that
// cannot be asked (a resource the apiserver does not serve) is an error
// answer no expectation matches.
func TestCanIAnswer(t *testing.T) {
	newFakeLab(t)
	cs := kubefake.NewClientset()
	var attrs []*authorizationv1.ResourceAttributes
	cs.PrependReactor("create", "selfsubjectaccessreviews", func(action clienttesting.Action) (bool, runtime.Object, error) {
		review, ok := action.(clienttesting.CreateAction).GetObject().(*authorizationv1.SelfSubjectAccessReview)
		if !ok {
			return true, nil, fmt.Errorf("created a %T", action.(clienttesting.CreateAction).GetObject())
		}
		attrs = append(attrs, review.Spec.ResourceAttributes)
		review.Status.Allowed = review.Spec.ResourceAttributes.Verb != verbCreate
		return true, review, nil
	})
	seen := stubClientsetFor(t, cs)
	user := &rest.Config{Host: fakeKindServer, BearerToken: "user-token"}

	if got := canIAnswer(user, []string{verbGet, resourceNodes}); got != "yes" {
		t.Errorf("get nodes = %q, want yes", got)
	}
	if got := canIAnswer(user, []string{verbCreate, resourceNamespaces}); got != "no" {
		t.Errorf("create namespaces = %q, want no", got)
	}
	if got := canIAnswer(user, []string{verbList, gvrPods.Resource, "--all-namespaces"}); got != "yes" {
		t.Errorf("list pods --all-namespaces = %q, want yes", got)
	}
	if got := canIAnswer(user, []string{verbCreate, gvrDeployments.Resource, "-n", testNS}); got != "no" {
		t.Errorf("create deployments -n demo = %q, want no", got)
	}
	for _, bad := range [][]string{{verbGet, "unicorns"}, {verbGet}} {
		if got := canIAnswer(user, bad); !strings.HasPrefix(got, "error: ") {
			t.Errorf("can-i %v = %q, want an error answer", bad, got)
		}
	}
	want := []*authorizationv1.ResourceAttributes{
		{Namespace: defaultNamespace, Verb: verbGet, Group: "", Resource: resourceNodes},
		{Namespace: defaultNamespace, Verb: verbCreate, Group: "", Resource: resourceNamespaces},
		{Namespace: "", Verb: verbList, Group: "", Resource: gvrPods.Resource},
		{Namespace: testNS, Verb: verbCreate, Group: gvrDeployments.Group, Resource: gvrDeployments.Resource},
	}
	if !reflect.DeepEqual(attrs, want) {
		t.Errorf("reviews sent = %+v, want %+v", attrs, want)
	}
	for _, cfg := range *seen {
		if cfg != user {
			t.Errorf("a review was not asked as the user's token config: %+v", cfg)
		}
	}
}

// TestIdentityLine: the identity line is the username and the groups the way
// kubectl's jsonpath printed them (a JSON list), or the probe's failure.
func TestIdentityLine(t *testing.T) {
	cs := kubefake.NewClientset()
	cs.PrependReactor("create", "selfsubjectreviews", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, &authenticationv1.SelfSubjectReview{Status: authenticationv1.SelfSubjectReviewStatus{
			UserInfo: authenticationv1.UserInfo{Username: "oidc:viewer@lab.local", Groups: []string{"oidc:viewers", "oidc:developers"}},
		}}, nil
	})
	stubClientsetFor(t, cs)
	if got, want := identityLine(&rest.Config{}), `oidc:viewer@lab.local  ["oidc:viewers","oidc:developers"]`; got != want {
		t.Errorf("identityLine = %q, want %q", got, want)
	}

	refused := kubefake.NewClientset()
	refused.PrependReactor("create", "selfsubjectreviews", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("Unauthorized")
	})
	stubClientsetFor(t, refused)
	if got := identityLine(&rest.Config{}); !strings.Contains(got, "whoami failed") || !strings.Contains(got, "Unauthorized") {
		t.Errorf("a refused probe reads %q, want the failure and its cause", got)
	}
}
