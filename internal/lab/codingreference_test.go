package lab

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func referenceView() *referencePullRequestView {
	v := &referencePullRequestView{
		Number: 7, State: "OPEN", IsDraft: true, HeadRefName: "agentlab-ref-ab12",
		Body:           "A hello-world program.\n\n" + referenceSessionMarker + " builder-1",
		ReviewDecision: referenceDecisionPending,
		Files:          []referenceFile{{Path: "apps/agentlab-hello-ab12/main.go"}, {Path: "apps/agentlab-hello-ab12/main_test.go"}},
	}
	v.Author.Login = "person"
	return v
}

func review(state, login, body string) referenceReview {
	r := referenceReview{State: state, Body: body}
	r.Author.Login = login
	return r
}

func TestJudgeReferencePullRequest(t *testing.T) {
	const app, branch = "apps/agentlab-hello-ab12", "agentlab-ref-ab12"
	require.NoError(t, judgeReferencePullRequest(referenceView(), "person", branch, app, "builder-1"))

	for name, change := range map[string]func(v *referencePullRequestView){
		"not a draft":           func(v *referencePullRequestView) { v.IsDraft = false },
		"state CLOSED":          func(v *referencePullRequestView) { v.State = "CLOSED" },
		"author not the person": func(v *referencePullRequestView) { v.Author.Login = "other-login" },
		"a bot author":          func(v *referencePullRequestView) { v.Author.IsBot = true },
		"another branch":        func(v *referencePullRequestView) { v.HeadRefName = "agentlab-ref-other" },
		"no test":               func(v *referencePullRequestView) { v.Files = v.Files[:1] },
		"outside the app":       func(v *referencePullRequestView) { v.Files = append(v.Files, referenceFile{Path: "main.go"}) },
		"no builder Session":    func(v *referencePullRequestView) { v.Body = "A hello-world program." },
		"another Session":       func(v *referencePullRequestView) { v.Body = referenceSessionMarker + " builder-2" },
	} {
		t.Run(name, func(t *testing.T) {
			v := referenceView()
			change(v)
			require.Error(t, judgeReferencePullRequest(v, "person", branch, app, "builder-1"))
		})
	}
}

func TestJudgeReferenceBuildTools(t *testing.T) {
	const greeting = "Hello from agentlab reference run ab12"
	require.NoError(t, judgeReferenceBuildTools("ok  \tgithub.com/giantswarm/agentlab-workspace-fixture/apps/agentlab-hello-ab12\t0.004s\n"+greeting+"\n", greeting))
	require.ErrorContains(t, judgeReferenceBuildTools(greeting, greeting), "go test")
	require.ErrorContains(t, judgeReferenceBuildTools("ok  \texample.com/x\t0.1s", greeting), "greeting")
	require.Error(t, judgeReferenceBuildTools("FAIL\texample.com/x\n--- FAIL: TestGreeting\nI ran the tests and they are ok", greeting))
}

func TestReferenceReplyNumber(t *testing.T) {
	n, err := referenceReplyNumber(probeMarker + " " + referenceProbePR + "=#42")
	require.NoError(t, err)
	require.Equal(t, 42, n)
	_, err = referenceReplyNumber("I opened the pull request.")
	require.Error(t, err)
	_, err = referenceReplyNumber(probeMarker + " " + referenceProbePR + "=none")
	require.Error(t, err)
}

func TestJudgeReferenceCommits(t *testing.T) {
	require.NoError(t, judgeReferenceCommits("abc1234567 admin@lab.local admin@lab.local\ndef1234567 admin@lab.local admin@lab.local\n", "admin@lab.local"))
	require.ErrorContains(t, judgeReferenceCommits("", "admin@lab.local"), "no commit")
	require.ErrorContains(t, judgeReferenceCommits("abc1234567 agent@example.com admin@lab.local", "admin@lab.local"), "agent@example.com")
	require.ErrorContains(t, judgeReferenceCommits("abc1234567 admin@lab.local bot@example.com", "admin@lab.local"), "bot@example.com")
}

func TestJudgeReferenceReview(t *testing.T) {
	body := "1. apps/agentlab-hello-ab12/main.go:7 return the greeting from a constant.\n\n" + referenceSessionMarker + " reviewer-1"
	ok := referenceView()
	ok.Reviews = []referenceReview{review(referenceReviewCommented, "person", body)}
	require.NoError(t, judgeReferenceReview(ok, "person", "reviewer-1"))

	short := referenceView()
	short.Reviews = []referenceReview{review(referenceReviewCommented, "person", "main_test.go: add a case.\n"+referenceSessionMarker+" reviewer-1")}
	require.NoError(t, judgeReferenceReview(short, "person", "reviewer-1"), "a file named by its base name and a line counts")

	for name, change := range map[string]func(v *referencePullRequestView){
		"no review":              func(v *referencePullRequestView) { v.Reviews = nil },
		"two reviews":            func(v *referencePullRequestView) { v.Reviews = append(v.Reviews, v.Reviews[0]) },
		"changes requested":      func(v *referencePullRequestView) { v.Reviews[0].State = "CHANGES_REQUESTED" },
		"review by someone else": func(v *referencePullRequestView) { v.Reviews[0].Author.Login = "other-login" },
		"no file": func(v *referencePullRequestView) {
			v.Reviews[0].Body = "Looks fine.\n" + referenceSessionMarker + " reviewer-1"
		},
		"an approval": func(v *referencePullRequestView) {
			v.Reviews = append(v.Reviews, review(referenceReviewApproved, "person", "LGTM"))
		},
		"approved decision":  func(v *referencePullRequestView) { v.ReviewDecision = "APPROVED" },
		"state MERGED":       func(v *referencePullRequestView) { v.State = "MERGED" },
		"no decision needed": func(v *referencePullRequestView) { v.ReviewDecision = "" },
	} {
		t.Run(name, func(t *testing.T) {
			v := referenceView()
			v.Reviews = []referenceReview{review(referenceReviewCommented, "person", body)}
			change(v)
			require.Error(t, judgeReferenceReview(v, "person", "reviewer-1"))
		})
	}
}

func TestJudgeReferenceBoundary(t *testing.T) {
	refused := "rc1 failed to create review: GraphQL: Review Can not approve your own pull request"
	gap, err := judgeReferenceBoundary(map[string]string{referenceProbeApprove: refused, referenceProbePerms: "admin,maintain,pull,push,triage"})
	require.NoError(t, err)
	require.Contains(t, gap, "admin,maintain,push")
	require.Contains(t, gap, "#39105")

	gap, err = judgeReferenceBoundary(map[string]string{referenceProbeApprove: refused, referenceProbePerms: "pull,triage"})
	require.NoError(t, err)
	require.Empty(t, gap)

	_, err = judgeReferenceBoundary(map[string]string{referenceProbeApprove: rcSuccess, referenceProbePerms: "pull"})
	require.ErrorContains(t, err, "accepted the reviewer's approval")
}

func TestReferenceTasksNameTheirSession(t *testing.T) {
	build := referenceBuildTask("giantswarm/agentlab-workspace-fixture", "apps/agentlab-hello-ab12", "agentlab-ref-ab12", "Hello from agentlab reference run ab12", "builder-1")
	require.Contains(t, build, referenceSessionMarker+" builder-1")
	require.Contains(t, build, "--draft")
	require.NotContains(t, build, "merge the")

	reviewTask := referenceReviewTask("giantswarm/agentlab-workspace-fixture", 7, "reviewer-1")
	require.Contains(t, reviewTask, referenceSessionMarker+" reviewer-1")
	require.Contains(t, reviewTask, "--comment")
	require.Contains(t, reviewTask, "Do not approve")
}
