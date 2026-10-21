package lab

import (
	"cmp"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/giantswarm/agentlab/internal/config"
	apiv1alpha1 "github.com/giantswarm/agentlab/internal/kagent/gen/kagent/api/v1alpha1"
)

// The harness stage of the workspaces proof: a Session admitted on the
// proof's workspace volume with a workspace grant, running one headless turn,
// on the claude Harness or another coding Harness the lab renders
// (--harness, such as codex). The rig is the lab's (workspacegrant.go): the
// workspace-manager signs with the lab's key and publishes it, kagent's jwt
// admission trusts that key set, and the Harness runs the Session.
//
//  1. The rig in place: the manager's key set publishes the lab's key, the
//     controller's admission names that key set, the manager's issuer and
//     the audience, and the Session's Harness is Ready.
//  2. The volume laid out as the workspace sync lays it out: a bare mirror
//     at mirrors/<owner>/<repository>.git.
//  3. An Agent Ready on the Session's Harness.
//  4. A Session without a volume: its turn works in its default directory
//     and sees no mirrors. Its creator is the person the grants name.
//  5. Refusals at create: a volume without a grant, a grant signed by a key
//     the manager does not publish, a grant for another person, and, naming
//     another namespace, a grant for another namespace's Agents and one
//     naming no Agent namespace.
//  6. A Session admitted with a grant on the volume: its turn works in its
//     own directory, sees the mirror at <owner>/<repository>.git read-only,
//     writes a file of its own, and that file is on the volume at
//     sessions/<Session id>. The directory holds the runtime's setup clone
//     of the mirror, and the agent's own git clone of the mirror succeeds as
//     the harness user.
//  7. The Sessions and the Agent removed.

const (
	// workspacesHarnessAgent is the proof's claude Agent (and its
	// AgentTemplate), deleted by the same run.
	workspacesHarnessAgent = "agentlab-workspaces-claude"
	// workspacesHarnessWorkspace is the workspace the grants name.
	workspacesHarnessWorkspace = "agentlab-workspaces-test"
	// workspacesHarnessRepository is the mirror the volume holds, as the
	// workspace sync names it: <owner>/<repository>.
	workspacesHarnessRepository = "agentlab/platform-api"
	// workspacesHarnessOwnFile is the file the Session's turn writes into its
	// own directory, read back from the volume.
	workspacesHarnessOwnFile = "agentlab-probe"
	// workspacesHarnessOtherNamespace is the Agent namespace a refused grant
	// names in place of the lab Agents' own.
	workspacesHarnessOtherNamespace = "agentlab-workspaces-other"
	// workspacesHarnessNamespaceRefusal is kagent's refusal of a grant
	// naming another Agent namespace or none.
	workspacesHarnessNamespaceRefusal = "admission grant is for another namespace"
	// workspacesHarnessGrantTTL is how long the proof's grants are valid,
	// the manager's default.
	workspacesHarnessGrantTTL = 5 * time.Minute
	// workspacesHarnessSystemPrompt keeps the turns to the probe; %s is the
	// runtime's shell tool.
	workspacesHarnessSystemPrompt = "You are a probe. Run exactly the command you are given with your %s, once, and reply with the output lines you are asked for, verbatim, nothing else."
	// workspaceManagerHTTPPort is the manager's HTTP port, where it serves
	// its key set.
	workspaceManagerHTTPPort = 8080
	// The kagent controller's ConfigMap and the keys its jwt admission reads.
	kagentControllerConfigMap = "kagent-controller"
	admissionJWKSURLKey       = "KAGENT_SESSION_ADMISSION_JWT_JWKS_URL"
	admissionIssuerKey        = "KAGENT_SESSION_ADMISSION_JWT_ISSUER"
	admissionAudienceKey      = "KAGENT_SESSION_ADMISSION_JWT_AUDIENCE"
	// kagentDefaultWorkingDirectory is where a Session without a volume works.
	kagentDefaultWorkingDirectory = "/data/workspace"
	// workspacesHarnessOwnClone is the directory the turn clones the mirror
	// into itself.
	workspacesHarnessOwnClone = "agentlab-own-clone"
)

// workspacesHarnessProbeCommand is the turn's Bash command: the working
// directory, the mirrors it sees as <owner>/<repository>.git, the clones in
// its directory as <owner>/<repository> (a kagent line that sets the
// directory up clones every mirror), whether the mirrors take a write, and
// a file of its own written and read back. Where the mirrors are mounted it
// also reads its uid and gid, clones the seeded mirror itself (the exit code,
// the clone's HEAD and git's output) and reads the setup clone's origin/HEAD.
func workspacesHarnessProbeCommand() string {
	return `echo "` + probeMarker + ` pwd=$(pwd)"; ` +
		`echo "` + probeMarker + ` mirrors=$(cd /mirrors 2>/dev/null && find . -mindepth 2 -maxdepth 2 -name '*.git' | sed 's|^\./||' | sort | tr '\n' ' ')"; ` +
		`echo "` + probeMarker + ` clones=$(find . -mindepth 3 -maxdepth 3 -name .git 2>/dev/null | sed 's|^\./||;s|/\.git$||' | sort | tr '\n' ' ')"; ` +
		`if [ ! -d /mirrors ]; then echo "` + probeMarker + ` mirrors-write=absent"; elif touch /mirrors/` + workspacesHarnessOwnFile + ` 2>/dev/null; then echo "` + probeMarker + ` mirrors-write=accepted"; else echo "` + probeMarker + ` mirrors-write=refused"; fi; ` +
		`if [ -d /mirrors ]; then ` +
		`echo "` + probeMarker + ` id=$(id -u):$(id -g)"; ` +
		`out=$(git clone /mirrors/` + workspacesHarnessRepository + `.git ./` + workspacesHarnessOwnClone + ` 2>&1); rc=$?; ` +
		`echo "` + probeMarker + ` own-clone=rc$rc head=$(git -C ./` + workspacesHarnessOwnClone + ` rev-parse --short HEAD 2>/dev/null) $(echo "$out" | tr '\n' ' ')"; ` +
		`echo "` + probeMarker + ` setup-clone=$(git -C ./` + workspacesHarnessRepository + ` rev-parse --short origin/HEAD 2>&1 | tr '\n' ' ')"; ` +
		`fi; ` +
		`echo session > ./` + workspacesHarnessOwnFile + ` && echo "` + probeMarker + ` own-write=$(cat ./` + workspacesHarnessOwnFile + `)"`
}

// workspacesSessionHarness is the stage's Session: the Harness it runs on,
// the ModelConfig its Agent uses, the Agent's name and the runtime's shell
// tool the prompts name.
type workspacesSessionHarness struct {
	name, modelConfig, agent, shellTool string
}

// workspacesSessionHarnessFor is the Session on harness (claude when empty)
// with modelConfig (default-model-config when empty on claude).
func workspacesSessionHarnessFor(harness, modelConfig string) workspacesSessionHarness {
	if harness == "" || harness == claudeHarness {
		return workspacesSessionHarness{name: claudeHarness, modelConfig: cmp.Or(modelConfig, defaultModelConfig), agent: workspacesHarnessAgent, shellTool: "Bash tool"}
	}
	return workspacesSessionHarness{name: harness, modelConfig: modelConfig, agent: "agentlab-workspaces-" + harness, shellTool: "shell tool"}
}

// workspacesHarnessProbePrompt asks the agent to run the command once with
// its shell tool and relay its marker lines.
func workspacesHarnessProbePrompt(shellTool string) string {
	return "Run this exact command once with your " + shellTool + ", then reply with every output line that starts with " + probeMarker +
		", verbatim and nothing else:\n\n" + workspacesHarnessProbeCommand()
}

// workspacesHarnessProbe is what a turn relayed.
type workspacesHarnessProbe struct {
	pwd, mirrorsWrite, ownWrite string
	mirrors, clones             []string
	// id is the turn's uid:gid; ownClone the agent's own clone of the
	// mirror as rc<exit> head=<HEAD> <git's output>; setupClone the setup
	// clone's origin/HEAD, or git's error.
	id, ownClone, setupClone string
}

var (
	probeLine     = regexp.MustCompile(probeMarker + ` ([a-z-]+)=([^\n]*)`)
	ownCloneLine  = regexp.MustCompile(`^rc(\d+) head=(\S*) ?(.*)$`)
	shortRevision = regexp.MustCompile(`^[0-9a-f]{7,40}$`)
)

// parseWorkspacesHarnessProbe reads the marker lines out of the reply.
func parseWorkspacesHarnessProbe(reply string) (workspacesHarnessProbe, error) {
	values := map[string]string{}
	for _, m := range probeLine.FindAllStringSubmatch(reply, -1) {
		values[m[1]] = strings.TrimSpace(strings.Trim(strings.TrimSpace(m[2]), "`"))
	}
	if values["pwd"] == "" || values["own-write"] == "" {
		return workspacesHarnessProbe{}, fmt.Errorf("the reply carries no %s pwd and own-write lines (did the agent run its shell tool?): %s", probeMarker, excerpt(reply, 300))
	}
	return workspacesHarnessProbe{
		pwd: values["pwd"], mirrorsWrite: values["mirrors-write"], ownWrite: values["own-write"],
		mirrors: strings.Fields(values["mirrors"]), clones: strings.Fields(values["clones"]),
		id: values["id"], ownClone: values["own-clone"], setupClone: values["setup-clone"],
	}, nil
}

func (p workspacesHarnessProbe) String() string {
	s := fmt.Sprintf("pwd %s, mirrors [%s], clones [%s], a write into the mirrors %s, its own file %q",
		p.pwd, strings.Join(p.mirrors, " "), strings.Join(p.clones, " "), p.mirrorsWrite, p.ownWrite)
	if p.id != "" {
		s += fmt.Sprintf(", uid:gid %s, its own clone %q, the setup clone %q", p.id, p.ownClone, p.setupClone)
	}
	return s
}

// checkOnVolume is the verdict on a Session admitted on the volume: its own
// directory, the mirror at <owner>/<repository>.git, the mirrors read-only,
// its own file written.
func (p workspacesHarnessProbe) checkOnVolume() error {
	var problems []string
	if p.pwd != workspacesTestMountPath {
		problems = append(problems, fmt.Sprintf("works in %s, not %s", p.pwd, workspacesTestMountPath))
	}
	if !containsString(p.mirrors, workspacesHarnessRepository+".git") {
		problems = append(problems, fmt.Sprintf("sees no mirror %s.git under /mirrors", workspacesHarnessRepository))
	}
	if p.mirrorsWrite != probeRefused {
		problems = append(problems, "the mirrors are not read-only ("+p.mirrorsWrite+")")
	}
	if p.ownWrite != "session" {
		problems = append(problems, "its own file reads "+strconv.Quote(p.ownWrite))
	}
	if len(problems) > 0 {
		return fmt.Errorf("the Session on the volume %s (%s)", strings.Join(problems, "; "), p)
	}
	return nil
}

// checkClones is the verdict on the Session's directory: the agent's own git
// clone of the seeded mirror succeeds, and the runtime's setup clone of it is
// in the directory. Its reading names both
// HEADs and the uid the agent cloned as.
func (p workspacesHarnessProbe) checkClones() (string, error) {
	var problems []string
	own := ownCloneLine.FindStringSubmatch(p.ownClone)
	switch {
	case own == nil:
		problems = append(problems, fmt.Sprintf("relayed no reading of its own clone of /mirrors/%s.git (%q)", workspacesHarnessRepository, p.ownClone))
	case own[1] != "0" || !shortRevision.MatchString(own[2]):
		problems = append(problems, fmt.Sprintf("its own git clone /mirrors/%s.git as uid:gid %s failed with exit %s: %s", workspacesHarnessRepository, p.id, own[1], strings.TrimSpace(own[3])))
	}
	if !shortRevision.MatchString(p.setupClone) {
		problems = append(problems, fmt.Sprintf("holds no setup clone of the mirror at ./%s (%s)", workspacesHarnessRepository, p.setupClone))
	}
	if len(problems) > 0 {
		return "", fmt.Errorf("the Session's directory: %s (%s)", strings.Join(problems, "; "), p)
	}
	return fmt.Sprintf("its own git clone of /mirrors/%s.git as uid:gid %s at %s, the setup clone ./%s at %s", workspacesHarnessRepository, p.id, own[2], workspacesHarnessRepository, p.setupClone), nil
}

// checkWithoutVolume is the verdict on a Session without a volume: its
// default directory, no mirrors.
func (p workspacesHarnessProbe) checkWithoutVolume() error {
	if p.pwd != kagentDefaultWorkingDirectory || len(p.mirrors) > 0 || p.mirrorsWrite != probeAbsent {
		return fmt.Errorf("the Session without a volume should work in %s with no /mirrors (%s)", kagentDefaultWorkingDirectory, p)
	}
	return nil
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// proveWorkspacesHarnessSession is the stage, on the proof's claim.
func proveWorkspacesHarnessSession(cfg *config.Config, harness workspacesSessionHarness, timeout time.Duration) (string, error) {
	ctx := context.Background()
	rig := workspaceAdmissionValuesFor(cfg, workspacesChartCarries(cfg))
	if rig == nil {
		skipped := "the " + harness.name + " Session skipped (no Session admission: the chart takes no workspaces values)"
		step("The %s Session on the volume: %s", harness.name, skipped)
		return skipped, nil
	}

	step("The rig: the workspace-manager publishes the lab's grant key, kagent's admission trusts its key set, the %s Harness is Ready", harness.name)
	key, err := readWorkspaceGrantKey()
	if err != nil {
		return "", err
	}
	kid, err := proveWorkspaceManagerPublishesGrantKey(ctx, key)
	if err != nil {
		return "", err
	}
	note("the manager's %s publishes the lab's key as %s", workspaceManagerJWKSPath, kid)
	if err := proveKagentAdmissionTrusts(ctx, rig); err != nil {
		return "", err
	}
	note("kagent's jwt admission: key set %s, issuer %s, audience %s", rig.JWKSURL, rig.Issuer, rig.Audience)
	if err := waitHarnessReady(ctx, harness.name, timeout); err != nil {
		return "", err
	}
	note("Harness %s/%s Ready", kagentNamespace, harness.name)

	step("The volume laid out as the workspace sync lays it out: the mirror %s.git", workspacesHarnessRepository)
	out, err := runPod(ctx, workspacesTestNamespace, workspacesTestPod("mirror", workspacesHarnessMirrorScript, []corev1.VolumeMount{{Name: workspacesTestVolume, MountPath: workspacesTestMountPath}}, false), timeout)
	if err != nil {
		return "", fmt.Errorf("the mirror pod: %w\n%s", err, out)
	}
	note("%s", indent(strings.TrimSpace(out), "  "))
	pv, err := workspacesClaimVolume(ctx, workspacesCSIDriverFor(cfg))
	if err != nil {
		return "", err
	}
	volume := workspaceVolume{Driver: pv.Spec.CSI.Driver, Handle: pv.Spec.CSI.VolumeHandle}

	step("A %s Agent %s on ModelConfig %s Ready on the Harness %s", harness.name, harness.agent, harness.modelConfig, harness.name)
	if agentExists(harness.agent) {
		if err := removeAgent(harness.agent); err != nil {
			return "", err
		}
	}
	defer func() {
		if err := removeAgent(harness.agent); err != nil {
			note("cleanup: %v", err)
		}
	}()
	if _, err := applyManifests(ctx, []byte(workspacesAgentManifest(harness.agent, harness.name, harness.modelConfig, fmt.Sprintf(workspacesHarnessSystemPrompt, harness.shellTool)))); err != nil {
		return "", fmt.Errorf("applying the %s Agent: %w", harness.name, err)
	}
	boot, err := waitAgentReadyOn(harness.agent, harness.name, timeout)
	if err != nil {
		return "", err
	}
	if !boot.ready {
		return "", fmt.Errorf("the %s Agent %s is not Ready on Harness %s after %s: %s", harness.name, harness.agent, harness.name, boot.elapsed.Round(time.Second), boot.reason)
	}
	note("Ready in %s", boot.elapsed.Round(time.Second))

	user := cfg.AdminUser()
	token, err := passwordGrant(cfg, config.AgentPlatformClientID, config.AgentPlatformClientSecret, user.Email, user.Password, musterLoginScopes)
	if err != nil {
		return "", err
	}
	api, err := dialKagentAPI(cfg, token)
	if err != nil {
		return "", err
	}
	defer api.close()
	sessions := &sessionSet{api: api}
	defer sessions.removeAll()
	controller := "of an unknown version"
	if v, err := api.version(ctx); err == nil {
		controller = v.GetKagentVersion()
	}

	step("A Session without a volume, as %s: its turn works in %s and sees no mirrors", user.Email, kagentDefaultWorkingDirectory)
	plain, probe, err := workspacesHarnessTurn(ctx, api, sessions, harness, &apiv1alpha1.CreateSessionRequest{}, timeout)
	if err != nil {
		return "", err
	}
	if err := probe.checkWithoutVolume(); err != nil {
		return "", err
	}
	person := plain.GetCreator()
	// kagent's admission compares the grant's sub with the Session's
	// creator, the person's email, which the manager's authorize_workspace
	// signs too.
	note("Session %s (creator %s): %s", plain.GetId(), person, probe)

	step("Refused at create: a volume without a grant, a grant the manager's key set does not verify, a grant for another person, a grant for another namespace's Agents and one naming none")
	source := workspacesHarnessVolumeSource(volume)
	foreign, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", err
	}
	for _, refusal := range []struct {
		what  string
		token func() (string, error)
		// reason is what kagent's refusal must name; empty takes any.
		reason string
	}{
		{"no grant", func() (string, error) { return "", nil }, ""},
		{"a grant signed by a key the manager does not publish", func() (string, error) {
			return signWorkspaceGrant(foreign, workspaceGrantJWK(&foreign.PublicKey).Kid, newWorkspaceGrant(rig.Issuer, person, workspacesHarnessWorkspace, volume, time.Now(), workspacesHarnessGrantTTL))
		}, ""},
		{"a grant for another person", func() (string, error) {
			return signWorkspaceGrant(key, kid, newWorkspaceGrant(rig.Issuer, "someone-else@lab.local", workspacesHarnessWorkspace, volume, time.Now(), workspacesHarnessGrantTTL))
		}, ""},
		{fmt.Sprintf("a grant for the Agents of namespace %s presented on Agent %s/%s", workspacesHarnessOtherNamespace, kagentNamespace, harness.agent), func() (string, error) {
			grant := newWorkspaceGrant(rig.Issuer, person, workspacesHarnessWorkspace, volume, time.Now(), workspacesHarnessGrantTTL)
			grant.AgentNamespace = workspacesHarnessOtherNamespace
			return signWorkspaceGrant(key, kid, grant)
		}, workspacesHarnessNamespaceRefusal},
		{"a grant without agent_namespace", func() (string, error) {
			return signGrantWithout(key, kid, newWorkspaceGrant(rig.Issuer, person, workspacesHarnessWorkspace, volume, time.Now(), workspacesHarnessGrantTTL), "agent_namespace")
		}, workspacesHarnessNamespaceRefusal},
	} {
		grant, err := refusal.token()
		if err != nil {
			return "", err
		}
		reason, err := workspacesHarnessRefused(ctx, api, harness.agent, source, grant)
		if err != nil {
			return "", fmt.Errorf("%s: %w", refusal.what, err)
		}
		if !strings.Contains(reason, refusal.reason) {
			return "", fmt.Errorf("%s: kagent %s refused for another reason than %q: %s", refusal.what, controller, refusal.reason, reason)
		}
		note("%s: %s", refusal.what, reason)
	}

	step("A Session admitted on the volume with the grant the manager's key verifies: its turn works in its own directory, the mirror %s.git read-only beside it", workspacesHarnessRepository)
	grant, err := signWorkspaceGrant(key, kid, newWorkspaceGrant(rig.Issuer, person, workspacesHarnessWorkspace, volume, time.Now(), workspacesHarnessGrantTTL))
	if err != nil {
		return "", err
	}
	onVolume, probe, err := workspacesHarnessTurn(ctx, api, sessions, harness, &apiv1alpha1.CreateSessionRequest{VolumeSource: source, AdmissionToken: grant}, timeout)
	if err != nil {
		return "", err
	}
	note("Session %s on %s %s: %s", onVolume.GetId(), volume.Driver, volume.Handle, probe)
	if err := probe.checkOnVolume(); err != nil {
		return "", fmt.Errorf("kagent %s: %w", controller, err)
	}
	clones, err := probe.checkClones()
	if err != nil {
		return "", fmt.Errorf("kagent %s: %w", controller, err)
	}
	clones = fmt.Sprintf("on kagent %s %s", controller, clones)
	note("%s", clones)

	step("The Session's file on the volume at %s/%s", workspacesTestSessions, onVolume.GetId())
	out, err = runPod(ctx, workspacesTestNamespace, workspacesTestPod("session-file", workspacesHarnessReadScript(onVolume.GetId()), []corev1.VolumeMount{{Name: workspacesTestVolume, MountPath: workspacesTestMountPath, ReadOnly: true}}, true), timeout)
	if err != nil {
		return "", fmt.Errorf("reading the Session's directory on the volume: %w\n%s", err, out)
	}
	note("%s", indent(strings.TrimSpace(out), "  "))

	if err := sessions.removeAllNow(); err != nil {
		return "", err
	}
	view := "the mirror " + workspacesHarnessRepository + ".git"
	if len(probe.clones) > 0 {
		view = "its clones " + strings.Join(probe.clones, " ")
	}
	return fmt.Sprintf("a %s Session admitted on the volume with the manager-verified grant (key %s) answered in its own directory with %s; %s; the refusals named", harness.name, kid, view, clones), nil
}

// workspacesHarnessMirrorScript lays the mirror out under
// mirrors/<owner>/<repository>.git, from the seed's repository.
const workspacesHarnessMirrorScript = workspacesGitSetup + `mkdir -p /workspace/` + workspacesTestMirrors + `/agentlab
git clone -q --bare /workspace/` + workspacesTestMirrors + `/` + workspacesTestRepo + ` /workspace/` + workspacesTestMirrors + `/` + workspacesHarnessRepository + `.git
echo "mirror ` + workspacesTestMirrors + `/` + workspacesHarnessRepository + `.git at $(git -C /workspace/` + workspacesTestMirrors + `/` + workspacesHarnessRepository + `.git rev-parse --short HEAD)"
`

// workspacesHarnessReadScript reads the Session's directory of the volume.
func workspacesHarnessReadScript(session string) string {
	dir := "/workspace/" + workspacesTestSessions + "/" + session
	return `test -d ` + dir + ` || { echo "no directory ` + workspacesTestSessions + `/` + session + ` on the volume"; exit 1; }
[ "$(cat ` + dir + `/` + workspacesHarnessOwnFile + `)" = session ] || { echo "the Session's file is not on the volume: $(ls -A ` + dir + ` | tr '\n' ' ')"; exit 1; }
echo "` + workspacesTestSessions + `/` + session + `/` + workspacesHarnessOwnFile + ` reads session; the directory holds: $(ls -A ` + dir + ` | tr '\n' ' ')"
`
}

// workspacesHarnessVolumeSource is the volume source the manager's grant
// allows: the Session's own directory read-write at /workspace, the mirrors
// read-only at /mirrors.
func workspacesHarnessVolumeSource(volume workspaceVolume) *apiv1alpha1.SessionVolumeSource {
	return &apiv1alpha1.SessionVolumeSource{
		Volume: &apiv1alpha1.SessionVolume{CsiDriver: volume.Driver, VolumeHandle: volume.Handle},
		Mounts: []*apiv1alpha1.SessionVolumeMount{
			{SubPath: workspaceGrantSessionDirectory, MountPath: workspacesTestMountPath},
			{SubPath: workspaceGrantMirrors, MountPath: "/mirrors", ReadOnly: true},
		},
	}
}

// workspacesHarnessAgentManifest is the proof's claude AgentTemplate on the
// default ModelConfig and its Agent on the claude Harness.
func workspacesHarnessAgentManifest(harness string) string {
	claude := workspacesSessionHarnessFor(claudeHarness, "")
	return workspacesAgentManifest(claude.agent, harness, claude.modelConfig, fmt.Sprintf(workspacesHarnessSystemPrompt, claude.shellTool))
}

// workspacesAgentManifest is a workspaces proof's AgentTemplate on the
// ModelConfig with the probe's system prompt, and its Agent of the same name
// on the Harness.
func workspacesAgentManifest(name, harness, modelConfig, systemPrompt string) string {
	return fmt.Sprintf(`apiVersion: %[1]s
kind: AgentTemplate
metadata:
  name: %[2]s
  namespace: %[3]s
  labels:
    %[4]s: %[5]s
spec:
  description: "Throwaway agent of agentlab workspaces-test; deleted by the same run."
  modelConfig:
    name: %[6]s
  systemPrompt: %[7]q
---
%[8]s`, agentTemplateAPIVersion, name, kagentNamespace, managedByLabel, managedByAgentlabValue, modelConfig, systemPrompt,
		agentManifest(name, name, harness, nil))
}

// workspacesHarnessTurn creates a Session of the stage's Agent from req and
// runs the probe turn on it.
func workspacesHarnessTurn(ctx context.Context, api *kagentAPI, sessions *sessionSet, harness workspacesSessionHarness, req *apiv1alpha1.CreateSessionRequest, timeout time.Duration) (*apiv1alpha1.Session, workspacesHarnessProbe, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req.Agent, req.RequestId = agentRef(harness.agent), uuid.NewString()
	session, err := api.createSessionWith(ctx, req)
	if session != nil && session.GetId() != "" {
		sessions.add(session.GetId())
	}
	if err != nil {
		return nil, workspacesHarnessProbe{}, err
	}
	t, err := api.turn(ctx, session, userMessage(workspacesHarnessProbePrompt(harness.shellTool)))
	if err != nil {
		return session, workspacesHarnessProbe{}, fmt.Errorf("the turn on Session %s: %w", session.GetId(), err)
	}
	reply, err := t.completedText()
	if err != nil {
		return session, workspacesHarnessProbe{}, fmt.Errorf("the turn on Session %s: %w", session.GetId(), err)
	}
	probe, err := parseWorkspacesHarnessProbe(reply)
	return session, probe, err
}

// workspacesHarnessRefused creates a Session of agent on the volume with
// grant and expects PermissionDenied: the controller's reason.
func workspacesHarnessRefused(ctx context.Context, api *kagentAPI, agent string, source *apiv1alpha1.SessionVolumeSource, grant string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, kubeReadTimeout)
	defer cancel()
	resp, err := api.sessions.CreateSession(api.callCtx(ctx), &apiv1alpha1.CreateSessionRequest{
		Agent: agentRef(agent), RequestId: uuid.NewString(), VolumeSource: source, AdmissionToken: grant,
	})
	if err == nil {
		id := resp.GetSession().GetId()
		api.removeSession(id)
		return "", fmt.Errorf("admitted as Session %s", id)
	}
	if status.Code(err) != codes.PermissionDenied {
		return "", fmt.Errorf("refused with %s, not PermissionDenied: %w", status.Code(err), err)
	}
	return status.Convert(err).Message(), nil
}

// proveWorkspaceManagerPublishesGrantKey reads the manager's key set through
// its pod and finds the lab's key in it: the key id the manager publishes
// it under.
func proveWorkspaceManagerPublishesGrantKey(ctx context.Context, key *ecdsa.PrivateKey) (string, error) {
	pod, err := readyPodOf(ctx, platformNamespace, "app.kubernetes.io/name=workspace-manager")
	if err != nil {
		return "", err
	}
	port, stop, err := portForwardPod(ctx, platformNamespace, pod, workspaceManagerHTTPPort)
	if err != nil {
		return "", err
	}
	defer stop()
	reqCtx, cancel := context.WithTimeout(ctx, kubeReadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d%s", port, workspaceManagerJWKSPath), nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("the manager's key set: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("the manager's %s answers %d (%s): a manager before 0.2.0, or no grant.signingKey", workspaceManagerJWKSPath, resp.StatusCode, excerpt(string(body), 200))
	}
	return publishedWorkspaceGrantKey(body, &key.PublicKey)
}

// proveKagentAdmissionTrusts reads the controller's admission settings and
// compares them with the rig's.
func proveKagentAdmissionTrusts(ctx context.Context, rig *workspaceAdmissionValues) error {
	k, err := labKube()
	if err != nil {
		return err
	}
	cm, err := k.clientset.CoreV1().ConfigMaps(kagentNamespace).Get(ctx, kagentControllerConfigMap, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("reading the controller's ConfigMap %s/%s: %w", kagentNamespace, kagentControllerConfigMap, err)
	}
	return admissionMatches(cm.Data, rig)
}

// admissionMatches compares the controller's admission settings with the
// rig's, naming every one that differs.
func admissionMatches(data map[string]string, rig *workspaceAdmissionValues) error {
	var problems []string
	for _, want := range []struct{ key, value string }{
		{admissionJWKSURLKey, rig.JWKSURL}, {admissionIssuerKey, rig.Issuer}, {admissionAudienceKey, rig.Audience},
	} {
		if got := data[want.key]; got != want.value {
			problems = append(problems, fmt.Sprintf("%s is %q, wanted %q", want.key, got, want.value))
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("kagent's Session admission is not the lab's: %s (`agentlab platform` renders it)", strings.Join(problems, "; "))
	}
	return nil
}

// waitHarnessReady waits for a Harness of the kagent namespace to report
// Ready.
func waitHarnessReady(ctx context.Context, name string, timeout time.Duration) error {
	gvr, err := gvrFor("harnesses.api.kagent.dev")
	if err != nil {
		return err
	}
	var last string
	ready := waitFor(int(timeout/pollInterval), pollInterval, func() bool {
		obj, err := getObject(ctx, gvr, kagentNamespace, name)
		if err != nil {
			last = err.Error()
			return false
		}
		var harness struct {
			Status agentStatus `json:"status"`
		}
		if err := decodeObject(obj, &harness); err != nil {
			last = err.Error()
			return false
		}
		s, message := harness.Status.condition("Ready")
		last = s + " " + message
		return s == conditionTrue
	})
	if !ready {
		return fmt.Errorf("the Harness %s/%s is not Ready after %s: %s", kagentNamespace, name, timeout, last)
	}
	return nil
}
