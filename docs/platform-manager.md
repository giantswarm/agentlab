# Platform manager

The lab carries a fixture for
[giantswarm-platform-manager](https://github.com/giantswarm/giantswarm-platform-manager),
the MCP server that enables, reconciles and verifies platform capabilities on
the installations of a registry, every write a pull request as the person. Its
render tests and `platformctl template` prove what a definition renders; the
fixture adds a live target: the released manager, signed in to through muster,
planning a real dry run against an installation with workspaces on.

## Turning it on

The fixture is opt-in:

```yaml
platform:
  platformManager:
    enabled: true
    version: 0.69.0   # optional: a release or release candidate of the chart and image; empty is the pinned default
```

Nothing runs until `agentlab pm-test`; the test refuses while the key is off.

## The fixture

`internal/lab/templates/platform-manager-fixture.yaml` is the registry the
manager reads, in the shape of the manager's own end-to-end registry: the
installations catalog (`agentlab/registry`, `catalog/installations.yaml`),
the installation's management-clusters and configs repositories, the fleet's
shared configs and collection base. It holds one invented installation,
`ember` of the invented customer `labco`, base domain `ember.agentlab.test`:

- the agent platform enabled on the 4 chart line, its own values turning
  workspaces on (`workspaces.enabled: true` with a `workspace-manager` block)
  in `installations/ember/apps/agent-platform/configmap-values.yaml.patch`;
- dex-app 3.3.0 pinned by its collection, the first version that takes the
  platform client's extra redirect URIs, so the commit is not refused;
- its own Dev Portal, and the hub the manager runs on.

`agentlab github-fake --platform-manager` serves it read-only, as a GitHub
Enterprise-shaped REST API (`/api/v3`): the repository, a file's contents, the
recursive tree at `HEAD`, the blobs by sha, empty release and commit lists, and
`GET /user` answering the login of the bearer — the lab Dex token's e-mail,
the token muster holds for the person. Every path it does not serve is a 404
recorded at `/_fake/misses`.

## pm-test

`agentlab pm-test [email]` (default: the admin user):

1. runs the registry as a container on the kind network, reached by the
   manager's pod through the Service `agentlab-pm-github`;
2. applies the manager's chart from `oci://gsoci.azurecr.io/charts/giantswarm/giantswarm-platform-manager`
   at the configured version as the HelmRelease `agentlab-platform-manager`
   (the platform's bundled engine), its GitHub and registry the fixture, its
   hub `ember`, OAuth on and pinned to the lab Dex through the OAuth fixture's
   client — the shape of the manager's own lab values — and waits for the
   MCPServer `giantswarm-platform-manager`;
3. signs the person in to it through muster (`core_auth_login`, the Dex form);
4. asserts `get_info` names the installed version;
5. runs `reconcile_capability` for `agent-platform` on `ember` as a dry run
   and asserts the workspaces case: `installation.workspaces.enabled` read
   from the record, `https://workspace-manager.ember.agentlab.test/signin` on
   the Dex client `muster` in the planned dex-app patch
   (`oidc.staticClients.muster.extraRedirectURIs`) and in the plan's Dex
   clients, the `workspaces` and `workspace-manager` values kept in the
   planned agent-platform patch, no commit refusal, and no write reaching the
   registry;
6. removes the release, the Service and the container.

A manager change for the workspaces case is proven by pointing
`platform.platformManager.version` at its release candidate and running
`agentlab pm-test`.
