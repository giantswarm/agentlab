package lab

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"strings"
	"testing"
	"time"
)

// TestGrantPayloadAndJudge: a grant the manager would sign decodes back into
// its claims, which the verdict accepts, and each claim changed is named.
func TestGrantPayloadAndJudge(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	volume := workspaceVolume{Driver: workspacesCSIDriver, Handle: "pvc-1"}
	g := newWorkspaceGrant("https://wm.example", testDevUser, accessWorkspace, volume, time.Now(), time.Minute)
	g.Changed = []string{syncMovingRepo}
	signed, err := signWorkspaceGrant(key, "kid", g)
	if err != nil {
		t.Fatal(err)
	}
	generic, claims, err := grantPayload(signed)
	if err != nil {
		t.Fatal(err)
	}
	if generic["sub"] != testDevUser || claims.Subject != testDevUser || claims.Expiry != g.Expiry {
		t.Fatalf("decoded %v / %+v, want the grant's subject and expiry", generic, claims)
	}
	if err := judgeGrant(claims, testDevUser, accessWorkspace, volume, []string{syncMovingRepo}); err != nil {
		t.Fatalf("the manager's grant is judged wrong: %v", err)
	}
	for _, tc := range []struct {
		person, workspace string
		volume            workspaceVolume
		changed           []string
		want              string
	}{
		{testPortalUser, accessWorkspace, volume, []string{syncMovingRepo}, "not " + testPortalUser},
		{testDevUser, accessOtherWorkspace, volume, []string{syncMovingRepo}, "names workspace"},
		{testDevUser, accessWorkspace, workspaceVolume{Driver: workspacesCSIDriver, Handle: "pvc-2"}, []string{syncMovingRepo}, "names volume"},
		{testDevUser, accessWorkspace, volume, nil, "changed since the sync"},
	} {
		err := judgeGrant(claims, tc.person, tc.workspace, tc.volume, tc.changed)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("judgeGrant(%s, %s, %s, %v) = %v, want %q", tc.person, tc.workspace, tc.volume.Handle, tc.changed, err, tc.want)
		}
	}
	claims.AgentNamespace = "another-tenant"
	if err := judgeGrant(claims, testDevUser, accessWorkspace, volume, []string{syncMovingRepo}); err == nil || !strings.Contains(err.Error(), "Agent namespace") {
		t.Errorf("a grant for another Agent namespace judged %v, want it named", err)
	}
	if _, _, err := grantPayload("not.a"); err == nil {
		t.Error("a token of two parts decoded as a grant")
	}
}

// TestSignWorkspaceClaimsResigned: a manager's claims signed again with one
// claim changed keep every other claim.
func TestSignWorkspaceClaimsResigned(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	g := newWorkspaceGrant("https://wm.example", testDevUser, accessWorkspace, workspaceVolume{Driver: workspacesCSIDriver, Handle: "pvc-1"}, time.Now(), time.Minute)
	signed, err := signWorkspaceGrant(key, "kid", g)
	if err != nil {
		t.Fatal(err)
	}
	generic, _, err := grantPayload(signed)
	if err != nil {
		t.Fatal(err)
	}
	generic["exp"] = time.Now().Add(-time.Minute).Unix()
	resigned, err := signWorkspaceClaims(key, "kid", generic)
	if err != nil {
		t.Fatal(err)
	}
	_, claims, err := grantPayload(resigned)
	if err != nil {
		t.Fatal(err)
	}
	if claims.Expiry >= time.Now().Unix() || claims.Subject != testDevUser || claims.Workspace[nameKey] != accessWorkspace {
		t.Errorf("the re-signed grant reads %+v, want the original's claims with an expiry in the past", claims)
	}
}

func TestJudgeNothingNew(t *testing.T) {
	before := accessTraces{kagent: []string{"s1"}, manager: []string{"s1"}, actors: []string{"a1"}}
	if err := judgeNothingNew(before, before); err != nil {
		t.Errorf("the same traces judged: %v", err)
	}
	gone := accessTraces{actors: []string{}}
	if err := judgeNothingNew(before, gone); err != nil {
		t.Errorf("fewer traces judged: %v", err)
	}
	after := accessTraces{kagent: []string{"s1", "s2"}, manager: []string{"s1", "s2"}, actors: []string{"a1", "a2"}}
	err := judgeNothingNew(before, after)
	if err == nil {
		t.Fatal("a new Session and actor not judged")
	}
	for _, want := range []string{"kagent holds a new Session s2", "list_sessions lists a new Session s2", "Substrate holds a new actor a2"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%v does not name %q", err, want)
		}
	}
}

func TestLatencySummary(t *testing.T) {
	if got := latencySummary(nil); got != "no calls" {
		t.Errorf("latencySummary(nil) = %q", got)
	}
	got := latencySummary([]time.Duration{30 * time.Millisecond, 10 * time.Millisecond, 20 * time.Millisecond})
	if got != "3 calls, min 10ms, median 20ms, max 30ms" {
		t.Errorf("latencySummary = %q", got)
	}
}
