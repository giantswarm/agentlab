package lab

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"

	ateapi "github.com/giantswarm/agentlab/internal/kagent/gen"
	apiv1alpha1 "github.com/giantswarm/agentlab/internal/kagent/gen/kagent/api/v1alpha1"
)

// CodingPartBrowser proves browser usability, refusals and the actor memory bound.
const CodingPartBrowser = "browser"

// CodingPartBrowserAttachment proves native GitHub screenshot uploads as the person.
const CodingPartBrowserAttachment = "browser-attachment"

const (
	browserSkillCommit = "7681d5c8056cb2f6cd30de3d2ec8e5fade81bfba"
	browserMemoryCase  = "memory"
	browserWorkerCase  = "worker"
	browserClosed      = "closed"
)

//go:embed templates/browser-proof.cjs
var browserProofScript string

func (p *codingProof) runBrowser() error {
	ctx := context.Background()
	names, err := p.prepareBrowser(ctx)
	if err != nil {
		return err
	}
	return p.browserChecks(ctx, names)
}

func (p *codingProof) runBrowserAttachment() error {
	ctx := context.Background()
	if _, err := p.prepareBrowser(ctx); err != nil {
		return err
	}
	return p.browserAttachment(ctx)
}

func (p *codingProof) prepareBrowser(ctx context.Context) ([]string, error) {
	names, err := toolchainNames(p.toolchains)
	if err != nil {
		return nil, err
	}
	if !slices.Contains(names, "node") || !slices.Contains(names, toolchainGitHub) {
		return nil, fmt.Errorf("the browser parts need node and github in --toolchains")
	}
	p.stages.begin("browser: harness", "The claude Harness Ready, with node and github toolchains")
	if err := waitHarnessReady(ctx, p.rt.name, p.timeout); err != nil {
		return nil, err
	}
	if err := p.connect(ctx); err != nil {
		return nil, err
	}
	model, err := p.model()
	if err != nil {
		return nil, err
	}
	p.removeAgents()
	p.stages.begin("browser: agent", "A claude agent with the pinned playwright-browser skill, created as %s", p.user.Email)
	if _, err := p.createReady(agentSpec{
		Name: codingTestAgent, Harness: p.rt.name, ModelConfig: model,
		Toolset: []string{"tool:" + codingTestReadTool},
		Skills: []agentSkill{{Name: "playwright-browser", Path: "playwright-browser", Git: &gitSkill{
			URL: "https://github.com/giantswarm/agent-skills.git", Commit: browserSkillCommit,
		}}},
	}); err != nil {
		return nil, err
	}
	return names, nil
}

func (p *codingProof) browserChecks(ctx context.Context, names []string) error {
	id, started, err := p.startToolchainSession(p.toolchains, names)
	if err != nil {
		return err
	}
	session, err := p.api.getSession(ctx, id)
	if err != nil {
		return err
	}
	p.stages.begin("browser: skill", "The agent serves a local page and uses the Playwright CLI through its skill")
	turn, err := p.turn(ctx, session, `Create a tiny web page titled "Agentlab browser proof" with a heading "Browser proof" and a button "Continue". Serve it on localhost:4317 in the background. Look at it in a browser, report its title and accessibility snapshot, and save a screenshot as /tmp/agentlab-browser-proof.png. Close the browser afterwards but keep the server running. Do not install anything.`)
	if err != nil {
		return err
	}
	var output strings.Builder
	for _, result := range turn.tools {
		output.WriteString(result.output)
	}
	if !strings.Contains(output.String(), "Browser proof") || !strings.Contains(output.String(), "Continue") {
		return fmt.Errorf("the browser turn's tool outputs contain no accessibility snapshot of the fixture")
	}

	p.stages.begin("browser: actor", "The actor mounts the exact toolchain digests and the target is its actor-JWT destination")
	ate, err := dialAteAPI(ctx)
	if err != nil {
		return err
	}
	defer ate.Close()
	if err := p.actorMounts(ctx, ate, id, started); err != nil {
		return err
	}
	actor, err := ate.GetActor(ctx, &ateapi.GetActorRequest{Actor: &ateapi.ObjectRef{Atespace: kagentNamespace, Name: substrateActorName(id)}})
	if err != nil {
		return err
	}
	template, err := ate.GetActorTemplate(ctx, &ateapi.GetActorTemplateRequest{ActorTemplate: actor.GetActorTemplate()})
	if err != nil {
		return err
	}
	target, err := browserSessionTarget(session)
	if err != nil {
		return err
	}
	note("browser target %s; actor limits %v", target, template.GetResources().GetLimits())
	memoryLimit := int64(0)
	for _, limit := range template.GetResources().GetLimits() {
		if limit.GetName() == browserMemoryCase {
			quantity, err := resource.ParseQuantity(limit.GetQuantity())
			if err != nil {
				return err
			}
			memoryLimit = quantity.Value()
		}
	}
	if memoryLimit <= 0 {
		note("the browser actor declares no memory limit; protocol checks still run and the memory assertion must fail")
	}
	p.stages.begin("browser: refusals", "No-route muster refusals and wrapper refusals for fetch, redirects, service workers and playwright test")
	command, err := browserProofCommand(target, memoryLimit)
	if err != nil {
		return err
	}
	turn, err = p.browserProtocolTurn(ctx, ate, session, target, command)
	if err != nil {
		return err
	}
	output.Reset()
	for _, result := range turn.tools {
		output.WriteString(result.output)
	}
	if err := judgeBrowserProof(parseCodingProbe(output.String())); err != nil {
		return fmt.Errorf("%w; tool output: %.3000s", err, output.String())
	}
	return nil
}

func browserSessionTarget(session *apiv1alpha1.Session) (string, error) {
	endpoint, err := url.Parse(defaultMusterMCPURL)
	if err != nil {
		return "", err
	}
	for _, provider := range session.GetProviders() {
		if provider.GetAudience() == componentMuster && strings.EqualFold(provider.GetScheme(), "Bearer") {
			endpoint.Host = net.JoinHostPort(provider.GetHostname(), endpoint.Port())
			return endpoint.String(), nil
		}
	}
	return "", fmt.Errorf("the browser Session has no muster bearer provider")
}

func (p *codingProof) browserProtocolTurn(ctx context.Context, client ateapi.ControlClient, session *apiv1alpha1.Session, target, command string) (codingTurn, error) {
	turnContext, cancel := context.WithTimeout(ctx, p.turnTimeout)
	defer cancel()
	done := make(chan struct{})
	var result codingTurn
	var turnError error
	go func() {
		defer close(done)
		result, turnError = p.turn(turnContext, session, "Run this exact browser protocol proof with your shell tool. Copy the compressed literal exactly. It sends MCP initialization requests only, never a tool call. Do not edit the script, expected checksum or substitute results. If checksum verification fails, recopy the original literal exactly. Reply with its AGENTLAB_PROBE lines.\n\n"+command)
	}()
	policyError := waitBrowserPolicy(turnContext, client, substrateActorName(session.GetId()), target, done)
	if policyError != nil {
		cancel()
	}
	<-done
	if policyError != nil {
		return codingTurn{}, policyError
	}
	return result, turnError
}

func waitBrowserPolicy(ctx context.Context, client ateapi.ControlClient, actor, target string, turnDone <-chan struct{}) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-turnDone:
			return fmt.Errorf("the browser turn ended without an observed actor-JWT rule for %s", target)
		default:
		}
		policy, err := client.GetActorEgressPolicy(ctx, &ateapi.GetActorEgressPolicyRequest{Actor: &ateapi.ObjectRef{Atespace: kagentNamespace, Name: actor}})
		if err != nil {
			return fmt.Errorf("read the browser actor's policy during its turn: %w", err)
		}
		if actual, err := browserMusterTarget(policy); err == nil {
			if actual != target {
				return fmt.Errorf("the browser targets %s, but its live actor-JWT rule targets %s", target, actual)
			}
			return nil
		}
		select {
		case <-turnDone:
			return fmt.Errorf("the browser turn ended without an observed actor-JWT rule for %s", target)
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func browserMusterTarget(policy *ateapi.EgressPolicy) (string, error) {
	for _, rule := range policy.GetRules() {
		http := rule.GetHttp()
		for _, header := range http.GetEffects().GetReplaceHeaders() {
			if header.GetActorJwt() == nil || !strings.EqualFold(header.GetHeader(), "Authorization") {
				continue
			}
			for _, host := range http.GetHostnames() {
				if strings.HasPrefix(host, "muster.") && !strings.Contains(host, "*") {
					ports := http.GetPorts().GetNumbers()
					if len(ports) == 1 {
						return fmt.Sprintf("http://%s:%d/mcp", host, ports[0]), nil
					}
				}
			}
		}
	}
	return "", fmt.Errorf("the actor's live egress policy has no exact muster HTTP destination with an actor-JWT Authorization replacement")
}

func browserProofCommand(target string, memoryLimit int64) (string, error) {
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write([]byte(browserProofScript)); err != nil {
		return "", fmt.Errorf("compress browser fixture: %w", err)
	}
	if err := writer.Close(); err != nil {
		return "", fmt.Errorf("finish browser fixture compression: %w", err)
	}
	return "bash <<'AGENTLAB_BROWSER_PROTOCOL'\nset -euo pipefail\numask 077\nscript=$(mktemp)\ntrap 'rm -f \"$script\"' EXIT\nprintf '%s' '" + base64.StdEncoding.EncodeToString(compressed.Bytes()) + "' | base64 -d | gzip -d > \"$script\"\nprintf '%s  %s\\n' '" + fmt.Sprintf("%x", sha256.Sum256([]byte(browserProofScript))) + "' \"$script\" | sha256sum -c -\nnode \"$script\" '" + target + "' '" + probeMarker + "' " + fmt.Sprint(memoryLimit) + "\nAGENTLAB_BROWSER_PROTOCOL", nil
}

func judgeBrowserProof(probe codingProbe) error {
	for _, key := range []string{"preflight", "anonymous", "extra-header", "fetch", "fetch-dot", "redirect", "redirect-dot", browserWorkerCase, "worker-dot", "suite", "suite-dot", browserMemoryCase} {
		want := "net::ERR_NAME_NOT_RESOLVED"
		switch key {
		case "preflight":
			want = "blocked"
		case "anonymous":
			want = "401"
		case "extra-header":
			want = "400"
		case browserMemoryCase:
			want = "bounded"
		}
		if got := probe["browser-"+key]; got != want {
			return fmt.Errorf("browser %s: got %q, want %q", key, got, want)
		}
	}
	return nil
}

func (p *codingProof) browserAttachment(ctx context.Context) error {
	p.stages.begin("browser: attachment", "A workspace Session uploads its screenshot to a draft proof PR as the person")
	gp := &gitProof{sp: newSyncProof(p.cfg, p.timeout, gitWorkspace), timeout: p.timeout, run: randomHex(4), toolchains: p.toolchains}
	defer gp.cleanup()
	if err := gp.preconditions("github"); err != nil {
		return err
	}
	if err := gp.createWorkspace(); err != nil {
		return err
	}
	grant, err := gp.admin.authorizeWorkspace(gitWorkspace)
	if err != nil {
		return err
	}
	if grant.Outcome != authorizeGranted || grant.Grant == "" {
		return fmt.Errorf("the screenshot Session needs a workspace grant: %s", grant.Outcome)
	}
	id, err := startAgentSession(gp.admin.session, codingTestAgent, grant.Grant, p.toolchains...)
	if err != nil {
		return err
	}
	gp.set.add(id)
	session, err := p.api.getSession(ctx, id)
	if err != nil {
		return err
	}
	branch := gitBranchPrefix + gp.run + "-browser"
	turn, err := p.turn(ctx, session, "Run this exact screenshot upload proof once with your shell tool. It creates a draft PR on the configured test repository, uploads only a synthetic local page, reads back the comment, and closes the PR. Do not edit the script.\n\n"+browserAttachmentCommand(gp.pr.pushed, branch))
	if err != nil {
		return err
	}
	var output strings.Builder
	for _, result := range turn.tools {
		output.WriteString(result.output)
	}
	probe := parseCodingProbe(output.String())
	if err := judgeBrowserAttachment(probe); err != nil {
		return fmt.Errorf("%w; tool output: %.2000s", err, output.String())
	}
	note("screenshot comment %s, posted as %s", probe["browser-comment"], probe["browser-person"])
	return nil
}

func browserAttachmentCommand(repository, branch string) string {
	return `bash <<'AGENTLAB_BROWSER_ATTACHMENT'
set -euo pipefail
gh=/opt/kagent/tools/github/bin/gh
"$gh" --version
"$gh" pr comment --help | grep -q -- --attach
cd './` + repository + `'
base=$(git symbolic-ref --short refs/remotes/origin/HEAD | sed 's|^origin/||')
git switch -q -c '` + branch + `' origin/HEAD
printf '%s\n' '` + branch + `' > .agentlab-browser-proof
git add .agentlab-browser-proof
git -c commit.gpgsign=false commit -qm 'test: browser screenshot proof'
pr=
server=
pushed=false
cleanup() {
  status=$?
  trap - EXIT
  playwright-cli close >/dev/null 2>&1 || true
  if test -n "$server"; then kill "$server" 2>/dev/null || true; fi
  if test -n "$pr"; then "$gh" pr close "$pr" --repo '` + repository + `' >/dev/null || status=1; fi
  if "$pushed"; then git push -q origin --delete '` + branch + `' >/dev/null || status=1; fi
  if test "$status" -eq 0; then echo '` + probeMarker + ` browser-close=closed'; fi
  exit "$status"
}
trap cleanup EXIT
git push -q origin 'HEAD:refs/heads/` + branch + `'
pushed=true
pr=$("$gh" pr create --draft --repo '` + repository + `' --head '` + branch + `' --base "$base" --title 'test: browser screenshot proof' --body 'Synthetic local browser screenshot uploaded from a workspace Session as the person.')
person=$("$gh" api user --jq 'select(.type == "User") | .login')
test -n "$person"
test "$("$gh" pr view "$pr" --repo '` + repository + `' --json author --jq .author.login)" = "$person"
node -e 'require("http").createServer((request,response)=>response.end("<title>Agentlab screenshot</title><h1>Browser proof</h1><button>Continue</button>")).listen(4318,"127.0.0.1")' >/tmp/agentlab-browser-server.log 2>&1 &
server=$!
node -e 'const http=require("http"); const check=()=>http.get("http://localhost:4318",response=>{response.resume();process.exit(response.statusCode===200?0:1)}).on("error",()=>setTimeout(check,100));check();setTimeout(()=>process.exit(1),10000)'
playwright-cli open http://localhost:4318/
playwright-cli screenshot --filename=/tmp/agentlab-browser-attachment.png
playwright-cli close
echo '` + probeMarker + ` browser-upload=attempted'
"$gh" pr comment "$pr" --repo '` + repository + `' --body 'Synthetic browser proof.' --attach '/tmp/agentlab-browser-attachment.png#Browser proof'
number=${pr##*/}
comment=$("$gh" api "repos/` + repository + `/issues/$number/comments" --jq '.[-1] | [.user.login,.html_url,.body] | @tsv')
printf '%s\n' "$comment" | grep -q 'https://github.com/user-attachments/assets/'
test "$(printf '%s' "$comment" | cut -f1)" = "$person"
echo '` + probeMarker + ` browser-person='"$person"
echo '` + probeMarker + ` browser-comment='"$(printf '%s' "$comment" | cut -f2)"
echo '` + probeMarker + ` browser-upload=attached'
AGENTLAB_BROWSER_ATTACHMENT
`
}

func judgeBrowserAttachment(probe codingProbe) error {
	comment, err := url.Parse(probe["browser-comment"])
	if err != nil || comment.Scheme != schemeHTTPS || comment.Host != "github.com" || !strings.HasPrefix(comment.Fragment, "issuecomment-") || probe["browser-person"] == "" || probe["browser-upload"] != "attached" || probe["browser-close"] != browserClosed {
		return fmt.Errorf("no screenshot attachment read back from GitHub as the person: %v", probe)
	}
	return nil
}
