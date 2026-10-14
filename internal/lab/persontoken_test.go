package lab

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	apiv1alpha1 "github.com/giantswarm/agentlab/internal/kagent/gen/kagent/api/v1alpha1"
)

// fakeSignIn is a person's sign-in whose tokens live for lifetime on a
// clock the test moves: issue mints a pair valid from now, and with
// renewAtExpiry the source answers the same pair until it has expired, the
// way muster's sign-in renews.
type fakeSignIn struct {
	clock         time.Time
	lifetime      time.Duration
	renewAtExpiry bool
	issued        int
	access, id    string
	expiry        time.Time
	err           error
	slept         []time.Duration
}

func (f *fakeSignIn) issue() {
	f.issued++
	f.expiry = f.clock.Add(f.lifetime)
	f.id = testJWT(map[string]any{claimSubject: testSubject, claimExpiry: f.expiry.Unix(), "n": f.issued})
	f.access = fmt.Sprintf("access-%d", f.issued)
}

func (f *fakeSignIn) fetch() (string, string, error) {
	if f.err != nil {
		return "", "", f.err
	}
	if f.id == "" || !f.renewAtExpiry || !f.clock.Before(f.expiry) {
		f.issue()
	}
	return f.access, f.id, nil
}

func (f *fakeSignIn) token() *personToken {
	p := newPersonToken(f.fetch)
	p.now = func() time.Time { return f.clock }
	p.sleep = func(d time.Duration) {
		f.slept = append(f.slept, d)
		f.clock = f.clock.Add(d)
	}
	return p
}

// valid says whether the ID token is unexpired on the sign-in's clock, the
// way the edge's JWT policy judges it.
func (f *fakeSignIn) valid(id string) bool {
	return f.clock.Before(tokenExpiry(f.clock, id))
}

// TestPersonTokenRenewsBeforeExpiry: a token is reused while it has time
// left and renewed shortly before it expires, never handed out expired.
func TestPersonTokenRenewsBeforeExpiry(t *testing.T) {
	f := &fakeSignIn{clock: time.Unix(1_800_000_000, 0), lifetime: 10 * time.Minute}
	p := f.token()
	first, err := p.idToken()
	if err != nil || f.issued != 1 {
		t.Fatalf("first: %v, issued %d", err, f.issued)
	}
	f.clock = f.clock.Add(9 * time.Minute)
	if again, _ := p.idToken(); again != first || f.issued != 1 {
		t.Errorf("a token with a minute left was renewed (issued %d)", f.issued)
	}
	f.clock = f.clock.Add(time.Minute - personTokenSkew + time.Second)
	renewed, err := p.idToken()
	if err != nil || renewed == first || f.issued != 2 || !f.valid(renewed) {
		t.Errorf("near its expiry: %v, renewed %v, issued %d", err, renewed != first, f.issued)
	}
	if access, _ := p.accessToken(); access != "access-2" {
		t.Errorf("the access token = %q, the renewed pair's is access-2", access)
	}
}

// TestPersonTokenWaitsForMusterRenewal: a sign-in that renews only once its
// tokens have expired is waited for, past the expiry, then asked again; one
// that does not renew then is an error that says to sign in again.
func TestPersonTokenWaitsForMusterRenewal(t *testing.T) {
	f := &fakeSignIn{clock: time.Unix(1_800_000_000, 0), lifetime: 10 * time.Minute, renewAtExpiry: true}
	p := f.token()
	first, _ := p.idToken()
	f.clock = f.clock.Add(10*time.Minute - 5*time.Second)
	renewed, err := p.idToken()
	if err != nil || renewed == first || f.issued != 2 || !f.valid(renewed) {
		t.Fatalf("at the expiry: %v, renewed %v, issued %d", err, renewed != first, f.issued)
	}
	if len(f.slept) != 1 || f.slept[0] != 6*time.Second {
		t.Errorf("waited %v, want once 6s (to a second past the expiry)", f.slept)
	}

	stuck := &fakeSignIn{clock: time.Unix(1_800_000_000, 0), lifetime: 10 * time.Minute}
	p = stuck.token()
	if _, err := p.idToken(); err != nil {
		t.Fatal(err)
	}
	p.fetch = func() (string, string, error) { return stuck.access, stuck.id, nil }
	stuck.clock = stuck.clock.Add(10 * time.Minute)
	if _, err := p.idToken(); err == nil || !strings.Contains(err.Error(), "sign in again") {
		t.Errorf("a sign-in that never renews: %v", err)
	}
}

// TestPersonTokenFailedRenewalTriesAgain: a renewal that fails is the
// call's error and is not remembered: the next call (the cleanup's) asks
// the sign-in again.
func TestPersonTokenFailedRenewalTriesAgain(t *testing.T) {
	f := &fakeSignIn{clock: time.Unix(1_800_000_000, 0), lifetime: 10 * time.Minute}
	p := f.token()
	if _, err := p.idToken(); err != nil {
		t.Fatal(err)
	}
	f.clock = f.clock.Add(10 * time.Minute)
	f.err = errors.New("no muster sign-in for https://muster.example (`muster auth login --endpoint https://muster.example` signs in)")
	if _, err := p.idToken(); err == nil || !strings.Contains(err.Error(), "muster auth login") {
		t.Fatalf("a failed renewal: %v", err)
	}
	f.err = nil
	if id, err := p.idToken(); err != nil || !f.valid(id) {
		t.Errorf("after the sign-in is back: %v", err)
	}
}

// TestPersonTokenRenewalRetriesTransientFailure: a renewal whose sign-in
// fails once is asked again after a pause and answers the renewed token; one
// that keeps failing ends after personTokenFetchAttempts, naming them and
// the cause.
func TestPersonTokenRenewalRetriesTransientFailure(t *testing.T) {
	f := &fakeSignIn{clock: time.Unix(1_800_000_000, 0), lifetime: 10 * time.Minute}
	p := f.token()
	if _, err := p.idToken(); err != nil {
		t.Fatal(err)
	}
	f.clock = f.clock.Add(10 * time.Minute)
	failures := 1
	p.fetch = func() (string, string, error) {
		if failures > 0 {
			failures--
			return "", "", errors.New("no muster sign-in for https://muster.example: exit status 2")
		}
		return f.fetch()
	}
	if id, err := p.idToken(); err != nil || !f.valid(id) || f.issued != 2 {
		t.Fatalf("a renewal failing once: %v, issued %d", err, f.issued)
	}
	if len(f.slept) != 1 || f.slept[0] != personTokenFetchBackoff {
		t.Errorf("paused %v, want once %s", f.slept, personTokenFetchBackoff)
	}

	f.slept = nil
	f.clock = f.clock.Add(10 * time.Minute)
	fetched := 0
	p.fetch = func() (string, string, error) {
		fetched++
		return "", "", errors.New("no muster sign-in for https://muster.example: exit status 2")
	}
	_, err := p.idToken()
	if err == nil || !strings.Contains(err.Error(), "4 attempts failed") || !strings.Contains(err.Error(), "exit status 2") {
		t.Errorf("a renewal that keeps failing: %v", err)
	}
	if fetched != personTokenFetchAttempts {
		t.Errorf("asked %d times, want %d", fetched, personTokenFetchAttempts)
	}
	want := []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second}
	if fmt.Sprint(f.slept) != fmt.Sprint(want) {
		t.Errorf("paused %v, want %v", f.slept, want)
	}
}

// TestPersonTokenOpaque: tokens without an exp claim are kept for
// personTokenOpaque, then asked for again.
func TestPersonTokenOpaque(t *testing.T) {
	clock, fetched := time.Unix(1_800_000_000, 0), 0
	p := newPersonToken(func() (string, string, error) { fetched++; return "opaque", "opaque", nil })
	p.now = func() time.Time { return clock }
	_, _, _ = p.tokens()
	clock = clock.Add(personTokenOpaque - personTokenSkew - time.Second)
	_, _, _ = p.tokens()
	if fetched != 1 {
		t.Errorf("fetched %d times within personTokenOpaque, want 1", fetched)
	}
	clock = clock.Add(2 * time.Second)
	_, _, _ = p.tokens()
	if fetched != 2 {
		t.Errorf("fetched %d times after personTokenOpaque, want 2", fetched)
	}
}

// TestKagentRenewingBearerAcrossExpiry: the person's token expires between
// two calls to the controller (a run's DeleteSession, then the cleanup's):
// each call carries the token current when it starts, never the dial's.
func TestKagentRenewingBearerAcrossExpiry(t *testing.T) {
	f := &fakeSignIn{clock: time.Unix(1_800_000_000, 0), lifetime: 16 * time.Minute, renewAtExpiry: true}
	person := f.token()
	k := newFakeKagent()
	for _, id := range []string{"s-run", "s-left"} {
		k.sessions[id] = &apiv1alpha1.Session{Id: id}
	}
	api := k.serve(t, "", renewingBearer(person.idToken)...)
	bearer := func() string {
		return strings.TrimPrefix(k.lastMD("DeleteSession").Get(authorizationMetadata)[0], "Bearer ")
	}
	if err := api.deleteSession(context.Background(), "s-run"); err != nil {
		t.Fatal(err)
	}
	first := bearer()
	f.clock = f.clock.Add(16 * time.Minute)
	set := &sessionSet{api: api, ids: []string{"s-left"}}
	if err := set.removeAllNow(); err != nil {
		t.Fatal(err)
	}
	second := bearer()
	if second == first || !f.valid(second) {
		t.Errorf("the cleanup's DeleteSession carried the expired token (renewed %v, valid %v)", second != first, f.valid(second))
	}
	if got := len(k.lastMD("DeleteSession").Get(authorizationMetadata)); got != 1 {
		t.Errorf("%d authorization values, want 1", got)
	}
	if len(k.sessions) != 0 {
		t.Errorf("sessions left: %v", k.sessions)
	}
}

// TestMusterSessionRenewsAcrossExpiry: a muster session that outlives the
// person's token sends the renewed one; a muster that refuses expired
// tokens answers every call.
func TestMusterSessionRenewsAcrossExpiry(t *testing.T) {
	f := &fakeSignIn{clock: time.Unix(1_800_000_000, 0), lifetime: 16 * time.Minute}
	person := f.token()
	var bearers []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		bearers = append(bearers, token)
		if !f.valid(token) {
			http.Error(w, `{"error":"invalid_token"}`, http.StatusUnauthorized)
			return
		}
		answer := rpcAnswer(1, map[string]any{})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(answer)
	}))
	t.Cleanup(srv.Close)
	s := &musterSession{client: srv.Client(), url: srv.URL, renew: person.idToken}
	if _, err := s.callTool("list_tools", nil); err != nil {
		t.Fatal(err)
	}
	f.clock = f.clock.Add(17 * time.Minute)
	if _, err := s.callTool("list_tools", nil); err != nil {
		t.Fatalf("after the first token expired: %v", err)
	}
	if len(bearers) != 2 || bearers[0] == bearers[1] {
		t.Errorf("the second call did not carry a renewed token")
	}
}
