# The migration rehearsal (3.x → 4.x in place)

A lab on a **released 3.x meta chart** — kagent 0.10 (`kagent.dev/v1alpha2`
`Agent`s) — seeded with the four shapes agents take on an installation,
upgraded **in place** with `agentlab platform` to the 4.x line (kagent API
v2, `kagent.dev/v1alpha3` `AgentTemplate`s on the platform Harness), and
agent-manager's migrate Job asserted through its three phases: **expand**
(the Generic-chart releases rewritten to chart 1.x values, the GitOps-owned
one reported as a diff), **contract** (the leftover v1alpha2 `Agent`s and the
five retired CRDs deleted) and **complete** (a re-run changes nothing). Then
every lab proof on the upgraded lab. The rehearsal is what an installation's
cut-over follows; the run this page records was made on agentlab v0.39.1
against meta chart 3.23.1 → 4.7.15, and found three platform defects on the
way (see [Observations](#observations)).

The scripts and manifests live in `hack/migration-rehearsal/`:

| File | Role |
|---|---|
| `seed.sh` | Stage 1: `agentlab down && up` on the 3.x chart, the four shapes seeded and asserted Accepted |
| `upgrade.sh` | Stage 2a: `agentlab platform` in place to the 4.x target, the platform asserted, run 1 of the migrate Job (expand), the operator's diff PR applied |
| `contract.sh` | Stage 2b: the releases on the 1.x agent chart, run 2 (contract) and run 3 (complete), one turn per migrated agent through the edge |
| `lib.sh` | Helpers the three share: logging, the status log, the lab lock, the Anthropic key, HelmRelease readers |
| `values-3x-example-agent.yaml` | 3.x overlay: the bundled `k8s-agent` example on (`kagent.k8s-agent.enabled` **and** `kagent.kagent-tools.enabled` — the agent binds the tool server the subchart renders) |
| `values-4x-migration.yaml` | 4.x overlay: `agentManager.migration.gitopsNamespaces: [flux-giantswarm]` — the namespaces whose releases the Job reports as a diff and never rewrites |
| `seed-narrow.yaml` | Shape 2: a hand-applied Generic-chart `HelmRelease` on 0.x values (`muster.toolNames`, `muster.serverRef.namespace: agent-platform`, `agent.runtime: python`) |
| `seed-gitops.yaml` | Shape 3 as seeded: the tenant namespace `flux-giantswarm`, its `OCIRepository agent` (`>=0.2.1 <1.0.0`) and `HelmRelease sre-agent` with the Kustomization ownership labels, `targetNamespace: kagent`, `driftDetection.ignore` on the two securityContext paths of the v1alpha2 `Agent` |
| `seed-gitops-1x.yaml` | Shape 3 after the operator's PR: the same objects with the Job's diff applied |

The four shapes, in the order the seed creates them:

| Shape | Agent | How it is owned | On 3.x |
|---|---|---|---|
| bundled example | `k8s-agent` | the meta chart's own `kagent` `HelmRelease` (values overlay) | an `Agent` no agent chart owns — nothing to migrate |
| portal-shaped | `sre` | created by agent-manager through muster, the way the portal does (skill `agent-self-awareness` at `ref: main`, `iconUrl`, `runtime: go`, toolset `[preset:read-only]`) | a Generic-chart 0.6.1 `HelmRelease` in `kagent` |
| hand-applied | `narrow` | `kubectl apply` of a Generic-chart `HelmRelease` (`muster.toolNames: [list_tools, call_tool]`) | a Generic-chart 0.6.1 `HelmRelease` in `kagent` |
| GitOps-owned | `flux-giantswarm/sre-agent` | a tenant's Flux Kustomization (stood in for by the ownership labels) | a Generic-chart 0.6.1 `HelmRelease` in `flux-giantswarm`, `targetNamespace: kagent` |

## The command sequence

The recipe needs **agentlab v0.39.1 or newer** — the release that renders the
3.x lab shape for a `chartVersion` below 4.0.0 (`config.LegacyChart`; see
[The agent platform](platform.md)) — and, for `contract.sh`, the release that
carries `agentlab turn` (the one this page ships with). A mikefarah `yq` v4,
`kubectl`, `helm`, `jq` and `kind` on the machine; the Anthropic key is read
from the Secret `kagent/kagent-anthropic` of the running lab unless
`ANTHROPIC_API_KEY` is set, and never printed.

```sh
cd <lab dir>                      # agentlab.yaml, state/, certs/
R=hack/migration-rehearsal        # of the agentlab checkout
E=/path/to/evidence               # one directory per stage below

# Stage 1 — the 3.x lab with the four shapes (CHART_VERSION defaults to 3.23.1).
# Takes the lab lock (state/lab-lock/owner) and holds it until the rehearsal ends.
$R/seed.sh $E/seed

# Stage 2a — in place to the 4.x target; the platform asserted; run 1 (expand);
# seed-gitops-1x.yaml compared with the report's diff and applied as the operator's PR.
$R/upgrade.sh $E/upgrade 4.7.15

# Stage 2b — the releases on chart 1.x, the templates Ready, run 2 (contract),
# run 3 (complete), one turn each as admin@lab.local. The second argument is
# the expand run's report, saved by stage 2a: the report ConfigMap holds only
# the latest run.
$R/contract.sh $E/contract $E/upgrade/run1/report.yaml

# Stage 3 — every proof on the upgraded lab.
agentlab platform-test && agentlab test && agentlab agents-test && agentlab toolsets-test \
  && agentlab skills-test && agentlab a2a-test && agentlab backstage-test \
  && agentlab klaus-gateway-test && agentlab models-test --backend ollama
```

Every wait is bounded. A failed assertion stops a script and leaves the lab
exactly as it is — nothing is restored or hand-patched, so the state can be
read. A platform defect (a failed Helm action of a component release, the
CRD storage-version refusal) stops `upgrade.sh` with exit 2 and a
`FOUND-ISSUE.md` in the evidence directory. `upgrade.sh` resumes at any step
on a lab already on the target (`START_STEP=assert-platform|migrate-run1|gitops-pr|record`,
and `REPORT_RUN1=<saved report>` when the Job ran again since);
`contract.sh` resumes at `START_STEP=1..6`. Each script appends its stage to
the lock owner file; the lock is released by hand when the rehearsal ends.

The proofs are **4.x-only** (they exercise the platform Harness, the Generic
chart 1.x, agent-manager 1.x): on the 3.x lab `platform-test` is not
applicable (its muster chain passes, the agent checks do not apply), and the
3.x lab is verified by the seed's own assertions — the chart versions of every
component release, the four `Agent`s Accepted with their Deployments rolled
out, the labels the ownership shapes carry.

## Timings

The run of 2026-09-11/12, lab on one machine (kind), agentlab v0.39.1:

| Step | Timing |
|---|---|
| `agentlab down` (the 4.x lab that was there) | 11 s |
| `agentlab up` on 3.23.1 (kagent wrapper 0.3.3 = kagent 0.10, agent-manager 0.4.5, backstage 0.244.8, connectivity 3.23.1; no first-revision failure) | 584 s |
| The shapes Accepted + Ready: `k8s-agent` / `sre` / `narrow` / `sre-agent` | 165 s / 9 s / 52 s / 18 s |
| `agentlab platform` 3.23.1 → 4.7.11 — **halted** on agent-platform#396: `kagent-crds` refused after 40 s, connectivity and kagent never moved, Helm's wait ran out | 930 s, then `helm rollback` to 3.23.1 33 s |
| `agentlab platform` 3.23.1 → 4.7.13 in place (the fix; `kagent-storage-version-backup` hook 7 s, `…-restore` hook 1 s) | 480 s |
| Re-pin 4.7.12 → 4.7.13 on the upgraded lab — **stalled** on agent-platform#399 (the migrate Job's immutable pod template) | — |
| Re-pin to 4.7.14 (the fix; the completed Job re-applied unchanged) | 84 s |
| Re-pin to 4.7.15 (agent-platform#401: both managers rolled with `--kagent-api-version=v1alpha3`) | 57 s |
| Migrate Job run 1 — `phase: expand` | 5 s |
| Flux: `sre-agent` Ready on chart 1.1.0 after the operator's PR (no schema refusal seen — the `OCIRepository` resolved `1.x` before the release reconciled) | 15 s |
| Flux: `sre`, `narrow` Ready on chart 1.x after the Job's rewrite | Ready before stage 2b began (since 23:28Z); their schema interval was not timed |
| `AgentTemplate sre-agent` Ready on Harness `kagent` (`sre`, `narrow` already Ready) | 10 s |
| Migrate Job run 2 — `phase: contract` | 6 s |
| Migrate Job run 3 — `phase: complete`, `changed: false` | 5 s |
| `agentlab turn --user admin@lab.local --template sre` (named its skill, 2+2=4) / `--template narrow` | 6 s / 3 s |
| `platform-test` / `test` / `agents-test` / `toolsets-test` / `skills-test` (golden boot 16 s) | 2 s / 0 s / 27 s / 71 s / 41 s |
| `a2a-test` / `backstage-test` (25 checks) / `klaus-gateway-test` (roster: narrow, sre, sre-agent) | 43 s / 74 s / 43 s |
| `models-test --backend ollama` — **failed** on 4.7.14 (agent-platform#401), passed on 4.7.15 (pull `qwen2.5:0.5b` 36 s → ModelConfig Accepted at v1alpha3 → turn → unload → delete) | 36 s failed / 63 s passed |

Every proof left the lab as it found it (the after-snapshot equal to the
before-snapshot, each time).

## What the Job rewrote, refused and deleted

**Rewritten** (run 1, `phase: expand`; the Generic-chart releases in `kagent`):

| Release | Removed | Renamed | Pinned | Range |
|---|---|---|---|---|
| `sre` | `agent.runtime` | — | skill `agent-self-awareness` `ref: main` → commit `cb1fb768bbbbcaa035b884a99ad308b14f846468` | — |
| `narrow` | `agent.runtime`, `muster.serverRef` | `muster.toolNames` → `muster.tools` | — | — |
| `OCIRepository kagent/agent` | — | — | — | `>=0.2.1 <1.0.0` → `1.x` |

`agent.iconUrl` and the toolsets were kept. The bundled `k8s-agent` was
reported `not-migratable` (no agent chart owns it).

**Refused** (never written): `flux-giantswarm/sre-agent` — `ownership:
external`, with the diff the operator applies to the owning repository:
`- runtime: python`, `- serverRef: {namespace: agent-platform}`,
`- toolNames:` / `+ tools:`, the two `driftDetection.ignore` paths
(`/spec/declarative/deployment/podSecurityContext`,
`/spec/declarative/deployment/securityContext` — paths of the retired
`Agent` kind), and the tenant's `OCIRepository` range `>=0.2.1 <1.0.0` →
`1.x`. `seed-gitops-1x.yaml` is that pull request; `upgrade.sh` compares it
line by line with the report's diff before it is applied, and the GitOps
objects stay byte-identical to the seed until then. Until the PR lands the
report names the release as pending and the contract phase does not run.

**Deleted** (run 2, `phase: contract`): the five retired CRDs —
`agents`, `sandboxagents`, `agentharnesses`, `memories`, `toolservers` (all
`.kagent.dev`). `agentsDeleted: []` — the 1.x chart renders the
`AgentTemplate` in the `Agent`'s place, so Helm had already replaced every
v1alpha2 `Agent` when the releases upgraded. The bundled `k8s-agent` went
with the 0.10 chart on the in-place upgrade (the 4.x line does not ship the
example; its overlay is dropped from `platform.valuesFiles`), not with the
contract. Run 3 (`phase: complete`, `changed: false`) proved the no-op.

## `platform-down` → `platform` is a reinstall, not the migration

`agentlab platform-down` uninstalls the meta chart release: the
`FluxInstance` goes with it, and with it every component `HelmRelease` — the
agents' releases included. A following `agentlab platform` on a 4.x version
installs a fresh platform with no v1alpha2 objects, no 0.x releases and a
migrate Job that finds nothing to do. That is a reinstall; it proves nothing
about the migration. The installation's path is the **in-place version
move**: `platform.chartVersion` in `agentlab.yaml` from the 3.x release to
the 4.x one, `platform.valuesFiles` from the 3.x overlay to the 4.x one, then
`agentlab platform` — Helm upgrades the one release, the components move in
dependency order, the hooks and the Job run. `upgrade.sh` does exactly that.

## What an installation's operator does differently

The lab has no Flux delivering the meta chart (Helm owns the release, see
[The agent platform](platform.md)); an installation's meta `HelmRelease`
moves the same way when its version bound is lifted. Beyond that, from the
chart's `UPGRADE.md` (3.x → 4.0) as the rehearsal exercised it:

- **Values to drop before the bound is lifted**: the `kagent.<example-agent>`
  blocks (the lab: `values-3x-example-agent.yaml` leaves `platform.valuesFiles`),
  `kagent.controller.skillsInitImage`, the `METRICS_*` entries of
  `kagent.controller.env`. The meta `HelmRelease` needs `spec.timeout: 12m`
  or more.
- **`agentManager.migration.gitopsNamespaces`**: every namespace GitOps-owned
  agents are applied from (the fleet: `[flux-giantswarm]`; the lab:
  `values-4x-migration.yaml`). Releases found there are reported, never
  rewritten. `agentManager.migration.dryRun: true` for one upgrade gives the
  report and the diffs with nothing written.
- **The kagent controller's DSN**: `kagent.controller.volumes[cnpg-dsn].secret.secretName: kagent-pg-kagent-v2-app`
  — the connectivity chart renders the second CNPG `Database` `kagent_v2` on
  the existing Cluster and derives that Secret from `kagent-pg-app` in a
  post-upgrade hook; the rehearsal saw both appear with the 4.x connectivity
  release (`upgrade.sh` asserts the `Database` Applied and the Secret's keys).
  The 0.10 database stays 30 days.
- **The token Secret**: private skill repositories are pinned through
  `kagent-skills-token` (key `token`) in the kagent namespace — the Secret the
  private-skill agents use; without it public repositories resolve and
  private refs are reported as pending. The lab's skill is public.
- **The diff PR**: the report's diff for each GitOps-owned release, applied
  to the owning repository as a pull request (values, the `OCIRepository`
  range, the `driftDetection.ignore` paths). Until it is merged the contract
  phase waits.
- **The re-run**: the contract phase (once every `AgentTemplate` is Ready and
  no release is left on 0.x), or any run after a fix, is the Job cloned under
  a new name — `kubectl create job --from` takes CronJobs only:

  ```sh
  J=$(kubectl -n kagent get job -l app.kubernetes.io/component=agent-manager-migrate -o name | head -1)
  kubectl -n kagent get "$J" -o json | jq 'del(.status, .metadata.uid, .metadata.resourceVersion,
      .metadata.creationTimestamp, .metadata.managedFields, .spec.selector,
      .spec.template.metadata.labels["batch.kubernetes.io/controller-uid"],
      .spec.template.metadata.labels["controller-uid"],
      .spec.template.metadata.labels["batch.kubernetes.io/job-name"],
      .spec.template.metadata.labels["job-name"]) | .metadata.name += "-rerun-1"' | kubectl create -f -
  ```

  `contract.sh` runs this twice (`agent-manager-migrate-2`, `-3`). The report:
  `kubectl -n kagent get configmap agent-manager-migrate-report -o yaml`.

## Observations

Everything the run taught that a cut-over must know:

- **agent-platform#396 → v4.7.13 — the kagent CRDs' storage version.** The
  kagent 0.10 wrapper's CRDs `modelconfigs`, `modelproviderconfigs` and
  `remotemcpservers.kagent.dev` store `v1alpha2`; the line's `kagent-crds`
  chart lists `v1alpha3` only, and the apiserver refuses the update
  (`storedVersions` must keep a served version). `kagent-crds` never installed,
  connectivity and kagent (`dependsOn: [kagent-crds]`) never moved, Helm's
  15-minute wait ran out (930 s) and `helm rollback` to 3.23.1 took 33 s. The
  fix is two hooks of the meta chart: `pre-upgrade` records the v1alpha2
  objects into ConfigMap `kagent/kagent-storage-version-migration`, deletes
  the three CRDs and re-points `kagent.dev/v1alpha2` → `v1alpha3` in every
  Helm release Secret's stored manifest (Helm reads a stored manifest back
  through a served version; the connectivity 3.x and kagent 0.10 releases had
  rendered v1alpha2 objects, so without the re-point they fail with `no
  matches for kind "RemoteMCPServer" in version "kagent.dev/v1alpha2"`);
  `post-upgrade` re-creates every hand-created `ModelConfig` at v1alpha3 (the
  only hand-added objects on installations; chart-rendered ones come back from
  their charts). In the run: backup hook 7 s, restore hook 1 s, the
  hand-created `ModelConfig manual-anthropic` back at v1alpha3 and Accepted.
- **agent-platform#399 → v4.7.14 — the migrate Job's identity across chart
  releases.** The Job's pod template carried the chart-version labels while
  its name hashed only image, args and env, so a chart patch re-applied a
  changed template to an immutable `spec.template` and stalled the
  connectivity release. Fixed with stable pod-template labels and a name that
  hashes the whole template: a re-pin takes 84 s and re-applies the completed
  Job unchanged; a template change renders a new Job.
- **agent-platform#401 → v4.7.15 — the managers keep the API version they
  started with.** model-manager (and agent-manager) discover the `kagent.dev`
  version once at start-up; their pods never rolled across the in-place
  upgrade (nothing in their values changed), so model-manager kept `v1alpha2`
  and every `ModelConfig` call failed with `the server could not find the
  requested resource` — `models-test` failed in 36 s. Fixed by pinning
  `kagent.apiVersion: v1alpha3` for both managers in the meta chart (the pin
  is the values change that rolls them; 57 s on the re-pin, both with
  `--kagent-api-version=v1alpha3`), after which `models-test` passed in 63 s.
  giantswarm/model-manager#77 asks for re-discovery on NotFound.
- **The report ConfigMap is a single slot.** `agent-manager-migrate-report`
  holds the latest run only; a re-run overwrites the expand report. Save it
  before cloning the Job (`upgrade.sh` saves it as `run1/report.yaml`;
  `contract.sh` takes that file as its second argument to assert the skill
  pin; `REPORT_RUN1` lets a resumed `upgrade.sh` assert from the copy).
- **A 3.x lab is verified by the seed's assertions**, because agentlab's
  proofs are 4.x-only (`platform-test` is not applicable on 3.x).
- **The bundled example needs its tool server**: `kagent.k8s-agent.enabled`
  alone leaves the `Agent` waiting for `RemoteMCPServer kagent-tool-server`;
  `kagent.kagent-tools.enabled` beside it. It took 165 s to Accepted, the
  slowest shape.
- **A hand-applied 0.x release names muster's namespace**:
  `muster.serverRef.namespace: agent-platform` on the Generic chart 0.6.1; the
  Job drops the key on 1.x.
- **Helm removes the v1alpha2 `Agent`s with the 1.x upgrades**: the contract
  phase found `agentsDeleted: []` and had only the CRDs to retire. An
  installation whose releases upgrade before the contract run sees the same;
  one where a release is still on 0.x sees the phase wait (`phase: wait`, the
  pending releases named) — `contract.sh` handles both.
- **The GitOps release upgraded without a schema interval**: the tenant's
  `OCIRepository` resolved `1.x` before the `HelmRelease` reconciled the new
  values (15 s to Ready on 1.1.0). When the release reconciles first, Flux
  reports `values don't meet the specifications of the schema` until the
  source catches up — expected and short.
- **The developer roster**: `agentlab turn --user dev@lab.local --list`
  returned all three templates while `kubectl auth can-i list agenttemplates`
  as that user is `no` — the controller's roster is not the user's RBAC.
  Recorded as an observation, not asserted.
- **Resumable stages**: `upgrade.sh` `START_STEP` (with `REPORT_RUN1`) and
  `contract.sh` `START_STEP=1..6` continue a stopped run from the evidence
  already on disk; the lab is never restored between attempts.
