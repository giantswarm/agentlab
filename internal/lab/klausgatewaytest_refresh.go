package lab

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/giantswarm/klaus-gateway/pkg/auth/musterlink"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/giantswarm/agentlab/internal/config"
)

// The refresh race proof (`klaus-gateway-test --part refresh`,
// docs/klaus-gateway.md "The refresh race"): two replicas of the meta chart's
// klaus-gateway component share the Secret link store, and both refresh the
// same person's muster refresh token at the same instant without signing the
// person out. muster rotates the refresh token on every refresh and treats a
// rotated token presented again as a replay: it revokes the token's whole
// family, the token it has just issued included. A gateway that lets two
// processes present one token therefore signs the person out; the gateway's
// refresh lease, written into the stored link, lets one process present it
// while the other waits and adopts the rotated link.
//
//   - a real sign-in: the person's `@bot login` gets the component's Sign in
//     button, whose link is driven headlessly through muster's authorize,
//     the Dex login form and muster back to the component's callback; the
//     link Secret then holds the person's record with a refresh token muster
//     issued;
//   - the Deployment at two replicas, both Ready, put back at its replicas on
//     every exit path;
//   - rounds: the record rewritten through the gateway's store package with
//     its cached id_token expired, the replicas' process-local copies left to
//     age past their cache TTL, then `@bot login` fired at both pods at the
//     same instant, each through its own port-forward. login probes the link
//     with TokenFor, so both pods refresh; each must answer "Signed in", and
//     the record must hold a rotated refresh token afterwards;
//   - muster's log over the run carries no refresh token reuse detection,
//     and a last `@bot login` still answers "Signed in";
//   - the person's record removed, a leftover of an aborted run first.
//
// A gateway without the refresh lease is expected to fail a round: both pods
// present the token, muster revokes the family, and one or both pods answer
// with the Sign in button.

// KlausGatewayPartRefresh is `klaus-gateway-test --part refresh`.
const KlausGatewayPartRefresh = "refresh"

const (
	// refreshRaceRounds is how many simultaneous refreshes the part fires.
	refreshRaceRounds = 5
	// refreshRaceReplicas is the replica count the part runs the component
	// at.
	refreshRaceReplicas = 2
	// refreshCacheWait outlasts the gateway's process-local link cache (30 s,
	// musterlink's linkCacheTTL): only a copy older than that is read from
	// the store again, so only then does a pod see the expired record.
	refreshCacheWait = 40 * time.Second
	// refreshReplyWait bounds a pod's answer to `@bot login`: a refresh, or a
	// wait for the other pod's lease (twice the gateway's 30 s refresh
	// timeout) and the adoption of its link.
	refreshReplyWait = 2 * time.Minute
	// refreshScaleWait bounds the scale to two Ready pods and back.
	refreshScaleWait = 3 * time.Minute
	// refreshSlackSuffix ends the part's Slack user id after the run.
	refreshSlackSuffix = "R"
	// httpsPort is the port of an https URL without one.
	httpsPort = "443"
)

// The gateway's answers to `@bot login` the part tells apart: the
// confirmation of a linked person ("Signed in as <email>." or "Signed in."),
// the refresh that failed without dropping the link, and the Sign in button
// (slackActionSignIn) of a person who is not linked.
const (
	loginSignedInText  = "Signed in"
	loginTokenErrorTxt = "could not be refreshed"
)

// The records of refresh token reuse in muster's log (mcp-oauth's server
// logger and its security audit events). muster rate-limits the first one,
// so each is looked for.
var refreshReuseMarkers = []string{
	"Refresh token reuse detected",
	"refresh_token_reuse_detected",
	"revoked_token_family_reuse_attempt",
	"Attempted use of revoked token family",
}

// loginOutcome is what a pod answered a person's `@bot login`.
type loginOutcome int

const (
	loginPending loginOutcome = iota
	loginLinked
	loginSignedOut
	loginTransient
)

func (o loginOutcome) String() string {
	switch o {
	case loginLinked:
		return "signed in"
	case loginSignedOut:
		return "the Sign in button (signed out)"
	case loginTransient:
		return "the sign-in could not be refreshed"
	default:
		return "no answer"
	}
}

// loginReplyIn is the gateway's answer to user's `@bot login` among a
// thread's messages: the Sign in button shown to them, the transient refresh
// failure, or the confirmation. loginPending while none is there.
func loginReplyIn(msgs []slackMessage, user string) (loginOutcome, slackMessage) {
	for _, m := range msgs {
		if _, found := m.action(slackActionSignIn); found && (m.Recipient == user || m.Method == slackPostMessage) {
			return loginSignedOut, m
		}
	}
	for _, m := range msgs {
		if m.Recipient != user {
			continue
		}
		shown := strings.TrimSpace(m.shown())
		switch {
		case strings.Contains(shown, loginTokenErrorTxt):
			return loginTransient, m
		case strings.HasPrefix(shown, loginSignedInText):
			return loginLinked, m
		}
	}
	return loginPending, slackMessage{}
}

// expiredLink is link with its cached id_token expired at now, everything
// else as stored: TokenFor serves no cached token from it and refreshes with
// its refresh token.
func expiredLink(link *musterlink.Link, now time.Time) *musterlink.Link {
	expired := *link
	expired.Expiry = now.Add(-time.Minute).UTC().Truncate(time.Second)
	return &expired
}

// roundVerdict judges one round from the pods' answers and the record before
// and after it: every pod answered signed in, and the record holds a refresh
// token muster rotated and an id_token that has not expired. A round no pod
// refreshed in proves nothing, so an unrotated token fails it too. The
// tokens themselves are never named.
func roundVerdict(before, after *musterlink.Link, answers map[string]loginOutcome, now time.Time) error {
	var refused []string
	for _, pod := range slices.Sorted(maps.Keys(answers)) {
		if answers[pod] != loginLinked {
			refused = append(refused, fmt.Sprintf("pod %s answered %s", pod, answers[pod]))
		}
	}
	if len(refused) > 0 {
		return fmt.Errorf("the person was not signed in on every pod: %s", strings.Join(refused, "; "))
	}
	switch {
	case after == nil:
		return errors.New("the person's record is gone from the link Secret")
	case after.RefreshToken == "":
		return errors.New("the person's record holds no refresh token")
	case after.RefreshToken == before.RefreshToken:
		return errors.New("the person's refresh token was not rotated: no pod refreshed, so the round raced nothing")
	case !after.Expiry.After(now):
		return fmt.Errorf("the person's record holds an id_token that expired at %s: the rotated link was not written", after.Expiry.UTC().Format(time.RFC3339))
	}
	return nil
}

// reuseDetections are the lines of muster's log that record a refresh token
// presented again.
func reuseDetections(logs string) []string {
	var found []string
	for _, line := range strings.Split(logs, "\n") {
		for _, marker := range refreshReuseMarkers {
			if strings.Contains(line, marker) {
				found = append(found, strings.TrimSpace(line))
				break
			}
		}
	}
	return found
}

// refreshFailures are the gateway's failed refresh records for slackUser in
// a pod's log.
func refreshFailures(logs, slackUser string) []string {
	var found []string
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, `"token_refresh"`) && strings.Contains(line, "refresh failed") && strings.Contains(line, slackUser) {
			found = append(found, strings.TrimSpace(line))
		}
	}
	return found
}

// edgeDialAddr is where this host dials addr: a host of the lab's domain on
// the https port goes to the edge's published port. The component's sign-in
// link, CIMD client_id and redirect URI carry no port (the hostname of a
// Gateway API route cannot), while the edge may be published elsewhere
// (platform.gatewayPort).
func edgeDialAddr(addr, domain, edgePort string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil || port != httpsPort || edgePort == httpsPort || domain == "" {
		return addr
	}
	if host == domain || strings.HasSuffix(host, "."+domain) {
		return net.JoinHostPort(host, edgePort)
	}
	return addr
}

// edgeTransport is the lab's transport with the edge's published port
// standing in for the https port of the lab's hostnames (edgeDialAddr).
func edgeTransport(cfg *config.Config) (*http.Transport, error) {
	base, err := labTLSTransport()
	if err != nil {
		return nil, err
	}
	transport := base.Clone()
	domain, edgePort := cfg.Platform.Domain, strconv.Itoa(cfg.Platform.GatewayPort)
	transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		return dialLab(ctx, network, edgeDialAddr(addr, domain, edgePort))
	}
	return transport, nil
}

// signedInLanding reports the component's post-sign-in landing: its callback
// at the code-free `?done=` address it redirects a completed sign-in to.
func signedInLanding(end *url.URL, _ []byte) bool {
	return end.Path == musterlink.CallbackPath && end.Query().Get("done") != ""
}

// refreshRace is one run of the part.
type refreshRace struct {
	ctx     context.Context
	cfg     *config.Config
	user    *config.User
	person  string
	signing string
	fake    slackWorkspace
	channel string
	store   *musterlink.SecretStore
}

// podDriver is one pod of the component as the part drives it: its name and
// a driver on a port-forward to it.
type podDriver struct {
	pod    string
	driver *slackDriver
	stop   func()
}

// klausGatewayRefreshTest is the part (see the file header).
func klausGatewayRefreshTest(cfg *config.Config, user *config.User, opts KlausGatewayTestOptions) error {
	if !cfg.KlausGatewayEnabled() {
		return fmt.Errorf("--part %s proves the meta chart's klaus-gateway component, which is off in this lab (platform.klausGateway; `agentlab configure --klaus-gateway` turns it on)", KlausGatewayPartRefresh)
	}
	ctx := context.Background()
	start := time.Now()
	k, err := labKube()
	if err != nil {
		return err
	}
	deploy, err := k.clientset.AppsV1().Deployments(platformNamespace).Get(ctx, klausGatewayComponent, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("reading %s: %w (platform.klausGateway is on; did `agentlab platform` install the component?)", describe(gvrDeployments, platformNamespace, klausGatewayComponent), err)
	}
	if err := assertStoreDeployment(deploy); err != nil {
		return err
	}
	if err := assertFakeSlackAPI(deploy); err != nil {
		return err
	}
	musterTrust, err := assertLabCA(deploy)
	if err != nil {
		return err
	}

	r := &refreshRace{ctx: ctx, cfg: cfg, user: user, person: slackUserPrefix + strings.ToUpper(randomSuffix()) + refreshSlackSuffix}
	r.channel = "CAGENTLAB" + strings.ToUpper(randomSuffix()) + refreshSlackSuffix
	step("Starting the fake Slack Web API as a container on the %s network and pointing the component's Service %s/%s at it; %s is %s",
		kindDockerNetwork, platformNamespace, klausGatewaySlackAPIService, r.person, user.Email)
	fake, err := startSlackFakeContainer(cfg, opts.SlackFakeBinary, map[string]string{r.person: user.Email}, nil)
	if err != nil {
		return err
	}
	defer fake.close()
	r.fake = fake
	removeService, err := fakeSlackForPods(cfg, fake.podIP, fakeContainerPort)
	if err != nil {
		return err
	}
	defer removeService()
	signing, err := secretDataKey(ctx, platformNamespace, klausGatewaySlackPlaceholder, slackSigningSecretKey)
	if err != nil {
		return err
	}
	r.signing = string(signing)

	key, err := secretDataKey(ctx, platformNamespace, klausGatewayOBOKeys, oboStoreKeyKey)
	if err != nil {
		return err
	}
	r.store, err = musterlink.NewSecretStore(k.clientset, key, musterlink.SecretStoreOptions{Namespace: platformNamespace, Name: klausGatewayLinksSecret},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		return fmt.Errorf("opening the link store through the gateway's package: %w", err)
	}
	links, err := k.clientset.CoreV1().Secrets(platformNamespace).Get(ctx, klausGatewayLinksSecret, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("reading the link Secret %s/%s: %w", platformNamespace, klausGatewayLinksSecret, err)
	}
	if removed, err := removeProofLinks(r.store, links.Data); err != nil {
		return err
	} else if removed > 0 {
		note("removed %d leftover record(s) of an earlier run", removed)
	}
	baseline, err := r.store.Check()
	if err != nil {
		return fmt.Errorf("the link store: %w", err)
	}
	defer func() {
		if err := r.store.Delete(r.person); err != nil && !errors.Is(err, musterlink.ErrNotLinked) {
			note("cleanup: removing the record of %s: %v", r.person, err)
		}
	}()

	pods, err := readyPods(ctx, k, deploy)
	if err != nil {
		return err
	}
	step("1. A real sign-in: %s's `@bot %s` through a port-forward to pod %s gets the Sign in button; its link is followed through muster's authorize, Dex's login form as %s and muster back to the component's %s",
		r.person, slackLoginWord, pods[0], user.Email, musterlink.CallbackPath)
	linked, err := r.signIn(pods[0], musterTrust)
	if err != nil {
		return err
	}
	note("the link Secret holds %s's record: %s, a refresh token muster issued, an id_token until %s", r.person, linked.Email, linked.Expiry.UTC().Format(time.RFC3339))

	original := int32(1)
	if deploy.Spec.Replicas != nil && *deploy.Spec.Replicas > 0 {
		original = *deploy.Spec.Replicas
	}
	step("2. Deployment %s at %d replicas (it runs %d), both Ready; put back at %d on every exit path", klausGatewayComponent, max(original, refreshRaceReplicas), original, original)
	if original < refreshRaceReplicas {
		restored := false
		defer func() {
			if !restored {
				if err := releaseGatewayWithin(ctx, original, refreshScaleWait); err != nil {
					note("cleanup: %v", err)
				}
			}
		}()
		if err := scaleGateway(ctx, refreshRaceReplicas); err != nil {
			return err
		}
		if err := r.raceAndCheck(k, deploy, start); err != nil {
			return err
		}
		restored = true
		if err := releaseGatewayWithin(ctx, original, refreshScaleWait); err != nil {
			return err
		}
	} else if err := r.raceAndCheck(k, deploy, start); err != nil {
		return err
	}

	if err := r.store.Delete(r.person); err != nil {
		return fmt.Errorf("removing the record of %s through the package: %w", r.person, err)
	}
	if count, err := r.store.Check(); err != nil {
		return fmt.Errorf("the link store after the cleanup: %w", err)
	} else if count != baseline {
		return fmt.Errorf("the link Secret holds %d entries after the part's record was removed, want the %d it had before", count, baseline)
	}
	fmt.Printf("\nPASS: %s signed in for real (gateway link -> muster -> Dex -> muster -> the component's callback); %d rounds of `@bot %s` fired at %d replicas at once, each refreshing the same expired link: every pod answered signed in, the refresh token was rotated each round, muster's log records no refresh token reuse; Deployment %s back at %d replicas, the record removed\n",
		user.Email, refreshRaceRounds, slackLoginWord, refreshRaceReplicas, klausGatewayComponent, original)
	return nil
}

// raceAndCheck runs steps 3 and 4 on the Deployment at two or more replicas.
func (r *refreshRace) raceAndCheck(k *kubeClients, deploy *appsv1.Deployment, start time.Time) error {
	pods, err := waitReadyPods(r.ctx, k, deploy, refreshRaceReplicas, refreshScaleWait)
	if err != nil {
		return err
	}
	pods = pods[:refreshRaceReplicas]
	note("pods %s Ready", strings.Join(pods, ", "))
	drivers := make([]*podDriver, 0, len(pods))
	defer func() {
		for _, d := range drivers {
			d.stop()
		}
	}()
	for _, pod := range pods {
		d, err := r.podDriver(pod)
		if err != nil {
			return err
		}
		drivers = append(drivers, d)
	}

	step("3. %d rounds: %s's record rewritten through pkg/auth/musterlink with its id_token expired, %s for the pods' link copies to age, then `@bot %s` at pods %s at the same instant",
		refreshRaceRounds, r.person, refreshCacheWait, slackLoginWord, strings.Join(pods, " and "))
	for round := 1; round <= refreshRaceRounds; round++ {
		if err := r.round(round, drivers); err != nil {
			return fmt.Errorf("round %d: %w", round, err)
		}
	}

	step("4. muster's log since the part started carries no refresh token reuse detection, and a last `@bot %s` at pod %s answers signed in", slackLoginWord, drivers[0].pod)
	logs, err := podLogs(r.ctx, platformNamespace, "deploy/"+componentMuster, componentMuster, time.Since(start)+time.Minute)
	if err != nil {
		return fmt.Errorf("reading muster's log: %w", err)
	}
	if found := reuseDetections(logs); len(found) > 0 {
		return fmt.Errorf("muster's log records %d refresh token reuse(s) since the part started: %s", len(found), excerpt(strings.Join(found, " | "), 600))
	}
	outcome, err := r.login(drivers[0])
	if err != nil {
		return err
	}
	if outcome != loginLinked {
		return fmt.Errorf("the last `@bot %s` at pod %s answered %s", slackLoginWord, drivers[0].pod, outcome)
	}
	note("no reuse in muster's log; pod %s still answers signed in", drivers[0].pod)
	return nil
}

// signIn drives the real sign-in through pod and returns the record it left.
func (r *refreshRace) signIn(pod, musterTrust string) (*musterlink.Link, error) {
	d, err := r.podDriver(pod)
	if err != nil {
		return nil, err
	}
	defer d.stop()
	proof := &slackProof{driver: d.driver, fake: r.fake, channel: r.channel}
	thread := r.fake.nextTS()
	if _, err := d.driver.mention(r.person, thread, slackLoginWord); err != nil {
		return nil, err
	}
	link, _, err := proof.awaitSignIn(r.person, thread, 0)
	if err != nil {
		return nil, fmt.Errorf("the unlinked %s's `@bot %s`: %w", r.person, slackLoginWord, err)
	}
	transport, err := edgeTransport(r.cfg)
	if err != nil {
		return nil, err
	}
	if err := completeSignInVia(transport, link, r.user, "the component's landing "+musterlink.CallbackPath+"?done=", signedInLanding); err != nil {
		return nil, fmt.Errorf("the sign-in from the component's link: %w (muster must accept the component's CIMD client on %s: `agentlab platform` with this agentlab lists that host in muster's allowPrivateIPClientMetadataHosts and sets allowPrivateIPRedirectURIs; the component dials %s)",
			err, klausGatewayOBOHost(r.cfg), musterTrust)
	}
	stored, err := r.store.Get(r.person)
	if errors.Is(err, musterlink.ErrNotLinked) {
		return nil, fmt.Errorf("the sign-in ended on the component's landing but the link Secret holds no record for %s", r.person)
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s's record: %w", r.person, err)
	}
	if stored.RefreshToken == "" {
		return nil, fmt.Errorf("%s's record holds no refresh token", r.person)
	}
	if !strings.EqualFold(stored.Email, r.user.Email) {
		return nil, fmt.Errorf("%s's record is linked to %s, want %s", r.person, stored.Email, r.user.Email)
	}
	return stored, nil
}

// round is one simultaneous refresh at every pod of drivers.
func (r *refreshRace) round(round int, drivers []*podDriver) error {
	before, err := r.store.Get(r.person)
	if err != nil {
		return fmt.Errorf("reading %s's record: %w", r.person, err)
	}
	if err := r.store.Put(r.person, expiredLink(before, time.Now())); err != nil {
		return fmt.Errorf("rewriting %s's record through the package: %w", r.person, err)
	}
	time.Sleep(refreshCacheWait)
	began := time.Now()
	threads := make([]string, len(drivers))
	for i := range drivers {
		threads[i] = r.fake.nextTS()
	}
	errs := fireTogether(len(drivers), func(i int) error {
		_, err := drivers[i].driver.mention(r.person, threads[i], slackLoginWord)
		return err
	})
	if err := errors.Join(errs...); err != nil {
		return err
	}
	answers := map[string]loginOutcome{}
	var shown []string
	for i, d := range drivers {
		outcome, msgs := r.awaitLogin(threads[i])
		answers[d.pod] = outcome
		if outcome != loginLinked {
			shown = append(shown, fmt.Sprintf("pod %s's thread: %s", d.pod, threadLine(msgs)))
		}
	}
	after, err := r.store.Get(r.person)
	if err != nil && !errors.Is(err, musterlink.ErrNotLinked) {
		return fmt.Errorf("reading %s's record after the round: %w", r.person, err)
	}
	if verdict := roundVerdict(before, after, answers, time.Now()); verdict != nil {
		var failures []string
		for _, d := range drivers {
			logs, _ := podLogs(r.ctx, platformNamespace, "pod/"+d.pod, klausGatewayComponent, time.Since(began)+10*time.Second)
			failures = append(failures, refreshFailures(logs, r.person)...)
		}
		detail := strings.Join(shown, "; ")
		if len(failures) > 0 {
			detail += "; the gateway's failed refreshes: " + excerpt(strings.Join(failures, " | "), 600)
		}
		if detail != "" {
			return fmt.Errorf("%w (%s)", verdict, detail)
		}
		return verdict
	}
	note("round %d: %d pods answered signed in %s after the simultaneous `@bot %s`; the refresh token rotated once, the id_token valid until %s",
		round, len(drivers), time.Since(began).Round(100*time.Millisecond), slackLoginWord, after.Expiry.UTC().Format(time.RFC3339))
	return nil
}

// login sends `@bot login` to one pod in a new thread and returns its answer.
func (r *refreshRace) login(d *podDriver) (loginOutcome, error) {
	thread := r.fake.nextTS()
	if _, err := d.driver.mention(r.person, thread, slackLoginWord); err != nil {
		return loginPending, err
	}
	outcome, _ := r.awaitLogin(thread)
	return outcome, nil
}

// awaitLogin waits for the gateway's answer to `@bot login` in thread.
func (r *refreshRace) awaitLogin(thread string) (loginOutcome, []slackMessage) {
	outcome := loginPending
	msgs, _ := waitThread(r.fake, r.channel, thread, refreshReplyWait, func(msgs []slackMessage) bool {
		outcome, _ = loginReplyIn(msgs, r.person)
		return outcome != loginPending
	})
	return outcome, msgs
}

// podDriver opens a port-forward to pod and a Slack driver on it, its
// connection opened ahead of the first event so a round's events leave
// together.
func (r *refreshRace) podDriver(pod string) (*podDriver, error) {
	local, stop, err := portForwardPod(r.ctx, platformNamespace, pod, gatewayHTTPPort)
	if err != nil {
		return nil, err
	}
	base := fmt.Sprintf("http://127.0.0.1:%d", local)
	driver := newSlackDriver(base, r.signing, r.fake, r.channel)
	if resp, err := driver.client.Get(base + "/"); err == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	return &podDriver{pod: pod, driver: driver, stop: stop}, nil
}

// fireTogether runs fire for 0..n-1, each in its own goroutine, released at
// the same instant, and returns their errors.
func fireTogether(n int, fire func(int) error) []error {
	errs := make([]error, n)
	release := make(chan struct{})
	var ready, done sync.WaitGroup
	ready.Add(n)
	done.Add(n)
	for i := range n {
		go func() {
			defer done.Done()
			ready.Done()
			<-release
			errs[i] = fire(i)
		}()
	}
	ready.Wait()
	close(release)
	done.Wait()
	return errs
}

// readyPods is the Deployment's Ready pods, at least one.
func readyPods(ctx context.Context, k *kubeClients, d *appsv1.Deployment) ([]string, error) {
	pods, err := deploymentPods(ctx, k, d)
	if err != nil {
		return nil, err
	}
	var ready []string
	for i := range pods {
		if podReady(&pods[i]) {
			ready = append(ready, pods[i].Name)
		}
	}
	if len(ready) == 0 {
		return nil, fmt.Errorf("%s has no Ready pod", describe(gvrDeployments, d.Namespace, d.Name))
	}
	slices.Sort(ready)
	return ready, nil
}

// waitReadyPods polls until the Deployment has n Ready pods and returns them.
func waitReadyPods(ctx context.Context, k *kubeClients, d *appsv1.Deployment, n int, timeout time.Duration) ([]string, error) {
	var ready []string
	if !waitFor(int(timeout/pollInterval), pollInterval, func() bool {
		var err error
		ready, err = readyPods(ctx, k, d)
		return err == nil && len(ready) >= n
	}) {
		return nil, fmt.Errorf("%s has %d Ready pod(s) after %s, want %d", describe(gvrDeployments, d.Namespace, d.Name), len(ready), timeout, n)
	}
	return ready, nil
}

// releaseGatewayWithin puts the Deployment back at its replicas and waits for
// the rollout within timeout.
func releaseGatewayWithin(ctx context.Context, replicas int32, timeout time.Duration) error {
	if err := scaleGateway(ctx, replicas); err != nil {
		return err
	}
	return waitDeploymentRolledOut(ctx, platformNamespace, klausGatewayComponent, timeout)
}
