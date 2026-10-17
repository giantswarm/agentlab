package lab

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/agentlab/internal/config"
	apiv1alpha1 "github.com/giantswarm/agentlab/internal/kagent/gen/kagent/api/v1alpha1"
)

// `agentlab workspaces-test --part write`: git and gh as the person from a
// workspace Session on the claude Harness, end to end. On the provider
// instance fake-bearer (the lab's GitHub shaped like github.com), as the
// admin:
//
//   - the admin grants the workspace App once in muster: core_auth_login on
//     the workspace-manager's MCPServer, walked through the lab GitHub's
//     consent to muster's callback;
//   - a workspace over agentlab-org/platform-api and a Session started
//     through start_session with the admin's grant, which agent-manager binds
//     to the admin in muster;
//   - one turn in the Session's clone: git switch -c agent/<token>, a commit,
//     git push and gh pr create; the lab's GitHub logs the push and gh's calls
//     as admin with the App's user token, which the egress gateway set from
//     muster (FetchSecret, ate-secret://muster.giantswarm.io/fake-bearer/…),
//     and the pull request's author is admin;
//   - the same turn pushes where GitHub refuses the person's App token: to
//     agentlab-org/handbook, which admin only reads, and to
//     agentlab-ext/scratch, whose owner the App is not installed on although
//     admin writes there; both refused with 403 naming admin;
//   - no GitHub token or JWT in the sandbox's environment, /data, /workspace
//     or the working directory, beyond the inert GH_TOKEN placeholder;
//   - a Session of the same Agent without a workspace pushes too: no
//     credential of admin's reaches the lab's GitHub.

// WorkspacesPartWrite is the write part of workspaces-test.
const WorkspacesPartWrite = "write"

const (
	// writeWorkspace is the part's workspace on fake-bearer.
	writeWorkspace = "agentlab-write"
	// writeAgent is the part's Agent on the claude Harness.
	writeAgent = "agentlab-workspaces-write"
	// writeRepo is the workspace's repository, which admin writes.
	writeRepo = "agentlab-org/platform-api"
	// writeReadOnlyRepo is a repository admin reads and never pushes to.
	writeReadOnlyRepo = "agentlab-org/handbook"
	// writeUninstalledRepo is a repository admin writes to, of an owner the
	// workspace App is not installed on.
	writeUninstalledRepo = "agentlab-ext/scratch"
	// writeBranchPrefix is where a coding agent proposes a change.
	writeBranchPrefix = "agent/"
	// musterCredentialAuthority is muster's credential provider authority
	// in the egress gateway's credential URIs.
	musterCredentialAuthority = "muster.giantswarm.io"
	// egressGatewayConfigSuffix ends the name of the egress gateway's
	// agentgateway ConfigMap, which lists its credential providers.
	egressGatewayConfigSuffix = "atenet-egress-agentgateway-config"
	// writePlaceholder is the inert GH_TOKEN gh sends when the Harness sets
	// none, for the egress gateway to replace.
	writePlaceholder = "agentlab-placeholder"
	// writeNoClone is the turn's clone line when the working directory holds
	// no clone of the workspace's repository.
	writeNoClone = "none"
)

// writeProof is the part's state on top of the caller-credential part's:
// the workspace, the Sessions and the Agent are made and removed the same
// way.
type writeProof struct {
	*callerCredentialProof
	gitHost string
	// granted says this part granted the workspace App in muster, which the
	// cleanup takes back; a grant the admin had before stays.
	granted bool
	branch  string
	pull    workspacePull
}

// workspacesWritePart runs the part in stages, as the admin.
func workspacesWritePart(cfg *config.Config, timeout time.Duration) error {
	p := newProofStages()
	wp := &writeProof{callerCredentialProof: &callerCredentialProof{sp: newSyncProof(cfg, timeout, writeWorkspace), timeout: timeout,
		apiHost: config.GitHubFakeBearerAPIHost(cfg.Platform.Domain), agent: writeAgent, workspace: writeWorkspace, repo: writeRepo},
		gitHost: config.GitHubFakeBearerHost(cfg.Platform.Domain), branch: writeBranchPrefix + "agentlab-write-" + randomHex(4)}
	err := wp.run(p)
	wp.cleanupWrite()
	if err == nil {
		fmt.Printf("\nWorkspaces write part passed: from a Session in workspace %s on the claude Harness, git pushed %s to %s and gh opened pull request #%d by %s, as the admin with the workspace App's token from muster; pushes to %s (read only) and %s (App not installed) refused with 403; no token in the sandbox beyond the GH_TOKEN placeholder; a Session without a workspace pushed with no credential.\n",
			writeWorkspace, wp.branch, writeRepo, wp.pull.Number, wp.pull.Author, writeReadOnlyRepo, writeUninstalledRepo)
	}
	return p.close(err)
}

func (wp *writeProof) run(p *proofStages) error {
	cfg := wp.sp.cfg
	adminUser := cfg.AdminUser()
	if adminUser == nil {
		return fmt.Errorf("the write part needs an admin user in %s (`agentlab configure --defaults` writes one)", config.File)
	}
	p.begin("write: preconditions", "The provider instance %s, muster as the egress gateway's credential provider %s, %s's grant of the workspace App in muster, the Session admission, the Agent %s on the claude Harness", config.WorkspaceProviderFakeBearer, musterCredentialAuthority, adminUser.Email, writeAgent)
	if !cfg.WorkspaceFakeBearerProvider() {
		return fmt.Errorf("the write part runs on the provider instance %s, and platform.workspaces.provider %q does not configure it: `agentlab configure --workspaces-provider %s,%s`, then `agentlab platform`",
			config.WorkspaceProviderFakeBearer, cfg.Platform.Workspaces.Provider, config.WorkspaceProviderFake, config.WorkspaceProviderFakeBearer)
	}
	if err := proveMusterCredentialProvider(context.Background()); err != nil {
		return err
	}
	admin, err := openWorkspacePerson(cfg, adminUser)
	if err != nil {
		return err
	}
	wp.admin = admin
	if err := wp.grant(); err != nil {
		return err
	}
	if err := wp.sp.preconditions(adminUser.Email); err != nil {
		return err
	}
	if err := wp.preconditions(adminUser); err != nil {
		return err
	}

	p.begin("write: workspace", "create_workspace %s with %s on %s, its first cycle", writeWorkspace, writeRepo, config.WorkspaceProviderFakeBearer)
	if err := wp.defineWorkspace(); err != nil {
		return err
	}

	p.begin("write: session", "A Session started through start_session with %s's grant, bound to the admin in muster by agent-manager", adminUser.Email)
	session, err := wp.startSession(true)
	if err != nil {
		return err
	}

	p.begin("write: git and gh as the person", "One turn in the Session's clone: git switch -c %s, a commit, git push, gh pr create; pushes to %s and %s refused; no token in the sandbox", wp.branch, writeReadOnlyRepo, writeUninstalledRepo)
	if err := wp.probe(session); err != nil {
		return err
	}

	p.begin("write: no workspace", "A Session of the same Agent without a workspace pushes to %s: no credential of the admin's reaches the lab's GitHub", writeRepo)
	bare, err := wp.startSession(false)
	if err != nil {
		return err
	}
	return wp.probeBare(bare)
}

// proveMusterCredentialProvider checks the egress gateway asks muster for a
// credential URI of muster's authority: the Substrate chart's
// credentialProvider.additionalProviders entry the agent-platform chart
// renders once muster serves FetchSecret.
func proveMusterCredentialProvider(ctx context.Context) error {
	cms, err := listObjects(ctx, gvrConfigMaps, substrateNamespace, "")
	if err != nil {
		return err
	}
	for _, cm := range cms {
		if !strings.HasSuffix(cm.GetName(), egressGatewayConfigSuffix) {
			continue
		}
		data, _ := cm.Object["data"].(map[string]any)
		file, _ := data["config.yaml"].(string)
		if !strings.Contains(file, "uriAuthority: "+musterCredentialAuthority) {
			return fmt.Errorf("the egress gateway (ConfigMap %s/%s) has no credential provider for %s: the gateway cannot ask muster for the person's token. It needs muster's FetchSecret endpoint and the agent-platform chart's substrate.credentialProvider.additionalProviders entry naming it, then `agentlab platform`",
				substrateNamespace, cm.GetName(), musterCredentialAuthority)
		}
		note("the egress gateway asks %s for its credential URIs (ConfigMap %s/%s)", musterCredentialAuthority, substrateNamespace, cm.GetName())
		return nil
	}
	return fmt.Errorf("no egress gateway configuration (ConfigMap *%s) in %s: is Substrate installed (`agentlab platform`)?", egressGatewayConfigSuffix, substrateNamespace)
}

// grant has the admin grant the workspace App in muster the way the portal's
// permission prompt does: core_auth_login on the workspace-manager's
// MCPServer, its challenge walked through the lab GitHub's consent to
// muster's callback. A grant the admin already holds answers no challenge.
func (wp *writeProof) grant() error {
	cfg := wp.sp.cfg
	env, err := wp.admin.session.callToolEnvelope("core_auth_login", map[string]any{serverKey: workspaceManagerMCPServer})
	if err != nil {
		return err
	}
	if env.IsError {
		return fmt.Errorf("core_auth_login for %s refused: %.400s: muster files the workspace App's grant through the workspace-manager's MCPServer, an OAuth server of the App (`agentlab platform` with a workspace-manager and chart that register it so)", workspaceManagerMCPServer, env.Content[0].Text)
	}
	if _, ok := env.StructuredContent["authUrl"].(string); !ok {
		note("%s holds the workspace App's grant in muster already: %.200s", wp.admin.user.Email, strings.TrimSpace(env.Content[0].Text))
		return nil
	}
	challenge, err := signInChallenge(cfg, wp.admin.session, workspaceManagerMCPServer)
	if err != nil {
		return err
	}
	client, err := workspaceSignInClient(cfg)
	if err != nil {
		return err
	}
	hops, err := walkSignIn(client, challenge.authURL, oauthProxyCallbackPath, wp.admin.login, wp.admin.user.Email, wp.admin.user.Password, nil)
	if err != nil {
		return fmt.Errorf("%s granting the workspace App in muster: %w", wp.admin.user.Email, err)
	}
	wp.granted = true
	note("%s granted the workspace App in muster: %s", wp.admin.user.Email, signInHops(hops))
	return nil
}

// writeValues is what the write turn relayed: exit codes with git's or gh's
// first telling line, and the names (never values) of the environment
// variables and files holding a token.
type writeValues struct {
	clone, push, pr, readOnly, uninstalled string
	ghToken                                string
	env, files                             []string
}

// parseWriteTurn reads the marker lines of a reply.
func parseWriteTurn(reply string, keys ...string) (writeValues, error) {
	values := map[string]string{}
	for _, m := range probeLine.FindAllStringSubmatch(reply, -1) {
		values[m[1]] = strings.TrimSpace(strings.Trim(strings.TrimSpace(m[2]), "`"))
	}
	for _, k := range keys {
		if _, ok := values[k]; !ok {
			return writeValues{}, fmt.Errorf("the reply carries no %s %s line (did the agent run its Bash tool?): %s", probeMarker, k, excerpt(reply, 300))
		}
	}
	return writeValues{clone: values["clone"], push: values["push"], pr: values["pr"], readOnly: values["read-only"], uninstalled: values["uninstalled"],
		ghToken: values["gh-token"], env: strings.Fields(values["token-env"]), files: strings.Fields(values["token-files"])}, nil
}

// writePurpose and writeExplain say what the write command does, for the
// model that runs it.
const (
	writePurpose = "the command proposes a one-file change on an agent/ branch of a repository of the lab's own fake GitHub with git and gh, tries two pushes the fake must refuse, and prints only exit codes, git's or gh's first error line, and the names (never the values) of variables and files that hold a token."
	writeExplain = "What it does: this is agentlab's test of git and gh as the person on a throwaway lab cluster. " + writePurpose + " The egress gateway, not you, supplies the credential; the test reads the result from the fake's own log."
)

// tokenPattern finds a GitHub token or a JWT in the sandbox.
const tokenPattern = `gh[opsu]_[A-Za-z0-9]{20,}|` + jwtPattern

// writeCommit is the shell that commits a file in the current directory on
// a new branch and pushes it to origin, quiet; its exit code is the push's,
// or the step's that failed.
func writeCommit(branch string) string {
	return `git switch -q -c '` + branch + `' && date > agentlab-write.txt && git add agentlab-write.txt && ` +
		`git -c user.name=agentlab -c user.email=agentlab@lab.local -c commit.gpgsign=false commit -qm 'agentlab write proof' && git push -q origin '` + branch + `'`
}

// firstLine words a command's output for a marker line: its first line that
// tells why, spaces as underscores.
const firstLine = `grep -m1 -iE 'denied|error|fatal|refused|http|https://' | tr ' ' '_'`

// writeCommand runs the turn: the change in the Session's clone of repo
// pushed and proposed, the two refused pushes in throwaway clones, and the
// token scan.
func writeCommand(gitHost, repo, branch string) string {
	m := `echo "` + probeMarker + ` `
	refused := func(key, r string) string {
		return `o=$(git clone -q 'https://` + gitHost + `/` + r + `.git' "$d/` + key + `" 2>&1 && cd "$d/` + key + `" && ` + writeCommit(branch) + ` 2>&1); ` +
			m + key + `=$?:$(echo "$o" | ` + firstLine + `)"; `
	}
	return m + `pwd=$(pwd)"; ` +
		`g=$(find . -maxdepth 4 -type d -path '*/` + repo + `/.git' 2>/dev/null | head -n 1); c=${g%/.git}; ` + m + `clone=${c:-` + writeNoClone + `}"; ` +
		`o=$(cd "${c:-/nonexistent}" && ` + writeCommit(branch) + ` 2>&1); ` + m + `push=$?:$(echo "$o" | ` + firstLine + `)"; ` +
		`o=$(cd "${c:-/nonexistent}" && GH_HOST='` + gitHost + `' GH_ENTERPRISE_TOKEN="${GH_ENTERPRISE_TOKEN:-${GH_TOKEN:-` + writePlaceholder + `}}" GH_TOKEN="${GH_TOKEN:-` + writePlaceholder + `}" ` +
		`gh pr create --repo '` + gitHost + `/` + repo + `' --head '` + branch + `' --base main --title 'agentlab write proof' --body 'Proposed by agentlab workspaces-test --part write.' 2>&1); ` +
		m + `pr=$?:$(echo "$o" | tail -n 1 | tr ' ' '_')"; ` +
		`d=$(mktemp -d); ` + refused("read-only", writeReadOnlyRepo) + refused("uninstalled", writeUninstalledRepo) + `rm -rf "$d"; ` +
		m + `gh-token=$([ -n "$GH_TOKEN" ] && echo set || echo unset)"; ` +
		m + `token-env=$(env | grep -E '` + tokenPattern + `' | cut -d= -f1 | sort -u | tr '\n' ' ')"; ` +
		m + `token-files=$(grep -rlE '` + tokenPattern + `' /data /workspace "$PWD" "$HOME/.config/gh" "$HOME/.git-credentials" 2>/dev/null | sort -u | tr '\n' ' ')"`
}

// writeBareCommand pushes from a Session without a workspace: a clone of
// repo, a commit, a push.
func writeBareCommand(gitHost, repo, branch string) string {
	m := `echo "` + probeMarker + ` `
	return `d=$(mktemp -d); o=$(git clone -q 'https://` + gitHost + `/` + repo + `.git' "$d/r" 2>&1 && cd "$d/r" && ` + writeCommit(branch) + ` 2>&1); ` +
		m + `push=$?:$(echo "$o" | ` + firstLine + `)"; rm -rf "$d"`
}

// probe runs the write turn on the Session in the workspace and judges it
// against the lab GitHub's request log and pull requests.
func (wp *writeProof) probe(session *apiv1alpha1.Session) error {
	before, err := wp.sp.gh.requests()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), wp.timeout)
	defer cancel()
	var v writeValues
	if _, _, err := probeTurnWith(ctx, wp.api, session, writeCommand(wp.gitHost, writeRepo, wp.branch), writeExplain, writePurpose, func(reply string) (err error) {
		v, err = parseWriteTurn(reply, "clone", "push", "pr", "read-only", "uninstalled", "token-env", "token-files")
		return err
	}); err != nil {
		return err
	}
	reqs, err := wp.sp.gh.requests()
	if err != nil {
		return err
	}
	window := reqs[min(len(before), len(reqs)):]
	note("%s: clone %s; push %s; gh pr create %s; read-only push %s; uninstalled push %s; GH_TOKEN %s", session.GetId(), v.clone, v.push, v.pr, v.readOnly, v.uninstalled, v.ghToken)
	note("the lab's GitHub logged: %s", gitRequestsLine(window, wp.gitHost))
	pulls, err := wp.sp.gh.pulls()
	if err != nil {
		return err
	}
	pull, err := judgeWrite(v, window, pulls, wp.gitHost, wp.admin.login, wp.branch)
	if err != nil {
		return err
	}
	wp.pull = pull
	note("%s: %s pushed to %s and pull request #%d opened by %s; %s and %s refused %s with 403; no token in the sandbox's environment or files",
		session.GetId(), wp.branch, writeRepo, pull.Number, pull.Author, writeReadOnlyRepo, writeUninstalledRepo, wp.admin.login)
	return nil
}

// judgeWrite is the verdict on the write turn: the push and gh's calls as
// login with the App's user token, the pull request of the branch by login,
// both refused pushes answered 403 naming login, no token in the sandbox.
func judgeWrite(v writeValues, window []githubRequest, pulls []workspacePull, gitHost, login, branch string) (workspacePull, error) {
	var none workspacePull
	switch {
	case v.clone == "" || v.clone == writeNoClone:
		return none, fmt.Errorf("the Session's working directory holds no clone of %s: the workspace's clone is missing", writeRepo)
	case len(v.env) > 0 || len(v.files) > 0:
		return none, fmt.Errorf("a token in the sandbox: environment variables %v, files %v; the person's token stays in muster and the egress gateway", v.env, v.files)
	}
	pushed := gitRequests(window, gitHost, writeRepo)
	if !strings.HasPrefix(v.push, "0:") || !slices.ContainsFunc(pushed, func(r githubRequest) bool {
		return strings.HasSuffix(r.Path, "/"+gitReceivePack) && r.Status == 200 && r.User == login && r.Credential == credUser
	}) {
		return none, fmt.Errorf("git push of %s to %s answered %s; the lab's GitHub logged %s, want git-receive-pack 200 as %s with the App's user token (credential none or placeholder: the egress gateway set no credential from muster for the Session's actor)",
			branch, writeRepo, v.push, gitRequestsLine(pushed, gitHost), login)
	}
	i := slices.IndexFunc(pulls, func(p workspacePull) bool { return p.Head == branch })
	if i < 0 {
		return none, fmt.Errorf("gh pr create answered %s, and the lab's GitHub holds no pull request of %s; its API calls: %s", v.pr, branch, gitRequestsLine(apiRequests(window, gitHost), gitHost))
	}
	if pulls[i].Author != login {
		return none, fmt.Errorf("the pull request #%d of %s is by %q, want %s: gh acted with someone else's credential", pulls[i].Number, branch, pulls[i].Author, login)
	}
	for _, c := range []struct{ repo, answer string }{{writeReadOnlyRepo, v.readOnly}, {writeUninstalledRepo, v.uninstalled}} {
		logged := gitRequests(window, gitHost, c.repo)
		if strings.HasPrefix(c.answer, "0:") || !slices.ContainsFunc(logged, func(r githubRequest) bool { return r.Status == 403 && r.User == login }) {
			return none, fmt.Errorf("the push to %s answered %s; the lab's GitHub logged %s, want a 403 to %s", c.repo, c.answer, gitRequestsLine(logged, gitHost), login)
		}
	}
	return pulls[i], nil
}

// probeBare pushes from the Session without a workspace and judges that no
// credential of the admin's reached the lab's GitHub.
func (wp *writeProof) probeBare(session *apiv1alpha1.Session) error {
	before, err := wp.sp.gh.requests()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), wp.timeout)
	defer cancel()
	var v writeValues
	branch := wp.branch + "-bare"
	if _, _, err := probeTurnWith(ctx, wp.api, session, writeBareCommand(wp.gitHost, writeRepo, branch), writeExplain, writePurpose, func(reply string) (err error) {
		v, err = parseWriteTurn(reply, "push")
		return err
	}); err != nil {
		return err
	}
	reqs, err := wp.sp.gh.requests()
	if err != nil {
		return err
	}
	window := reqs[min(len(before), len(reqs)):]
	note("%s, no workspace: push %s; the lab's GitHub logged %s", session.GetId(), v.push, gitRequestsLine(window, wp.gitHost))
	return judgeBare(v, window, session.GetId())
}

// judgeBare is the verdict on a Session without a workspace: no request
// carried a user's token, and its push failed.
func judgeBare(v writeValues, window []githubRequest, session string) error {
	for _, r := range window {
		if r.Credential == credUser {
			return fmt.Errorf("the Session %s without a workspace reached the lab's GitHub as %s with a user token (%s %s %d): a Session without a workspace is never bound to a person", session, r.User, r.Method, r.Path, r.Status)
		}
	}
	if strings.HasPrefix(v.push, "0:") {
		return fmt.Errorf("the Session %s without a workspace pushed (%s) with no user token logged: the push went through on a credential the proof does not know", session, v.push)
	}
	note("%s, no workspace: no credential of a person reached the lab's GitHub, the push refused", session)
	return nil
}

// gitRequests are the logged requests to repo on the git host, by git or
// its API.
func gitRequests(reqs []githubRequest, host, repo string) []githubRequest {
	var out []githubRequest
	for _, r := range reqs {
		if r.Host == host && (strings.HasPrefix(r.Path, "/"+repo+".git/") || strings.HasPrefix(r.Path, "/"+repo+"/")) {
			out = append(out, r)
		}
	}
	return out
}

// apiRequests are the logged REST and GraphQL calls on the git host.
func apiRequests(reqs []githubRequest, host string) []githubRequest {
	var out []githubRequest
	for _, r := range reqs {
		if r.Host == host && strings.HasPrefix(r.Path, "/api/") {
			out = append(out, r)
		}
	}
	return out
}

// gitRequestsLine words logged requests on the git host: method, path,
// status, user and credential kind.
func gitRequestsLine(reqs []githubRequest, host string) string {
	var words []string
	for _, r := range reqs {
		if r.Host == host {
			words = append(words, fmt.Sprintf("%s %s %d user=%s credential=%s", r.Method, r.Path, r.Status, r.User, r.Credential))
		}
	}
	if len(words) == 0 {
		return "no request"
	}
	return strings.Join(words, "; ")
}

// cleanupWrite removes what the caller-credential cleanup removes and takes
// back the grant this part made.
func (wp *writeProof) cleanupWrite() {
	wp.cleanup()
	if wp.granted && wp.admin != nil {
		if _, err := wp.admin.session.callToolEnvelope("core_auth_logout", map[string]any{serverKey: workspaceManagerMCPServer}); err != nil {
			warn("taking back %s's grant of the workspace App: %v", wp.admin.user.Email, err)
		}
	}
}
