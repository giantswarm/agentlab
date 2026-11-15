# Secret sources

The lab's real credentials — the Anthropic key, a GitHub token, the skills
GitHub App's private key and client secret, the workspace providers' keys —
never enter `agentlab.yaml` or `state/`. `agentlab.yaml` records a
*reference* to where each one lives, its **source**; `up` and `platform`
read the value from there and write it into the one key of the lab Secret its
consumer reads, so a recreated lab (`down`, `up`) carries its credentials
again without a manual step.

| Setting | `configure` flag | Lab Secret |
|---|---|---|
| `aiKey.source` | `--ai-key-source` | `kagent/kagent-anthropic` and the portal's `agent-platform/agentlab-anthropic` ([Agents](agents.md#the-model)) |
| `githubToken.source` | `--github-token-source` | `agent-platform/agentlab-github-token` ([Agents](agents.md#the-github-token)) |
| `githubApp.privateKeySource`, `clientSecretSource` | `--github-app-private-key-source`, `--github-app-client-secret-source` | `agent-platform/agentlab-github-app`, `agent-platform/agentlab-portal-github-app` |
| `platform.workspaces.github.*Source`, `platform.workspaces.gitlab.*Source` | `--workspaces-github-private-key-source`, … | `agent-platform/workspace-github`, `agent-platform/workspace-gitlab` ([Workspaces](workspaces.md)) |

## The reference forms

| Reference | Read through | What it needs |
|---|---|---|
| `op://<vault>/<item>/<field>` | `op read --no-newline <ref>` | The [1Password CLI](https://developer.1password.com/docs/cli/get-started/) signed in as you. With the desktop app's CLI integration, the app may ask you to approve the read. |
| `<file>#<dotted.path>` (`sops://` in front optional) | `sops --decrypt --output-type json <file>`, the path walked in agentlab's process | [sops](https://github.com/getsops/sops/releases) and a key that decrypts the file, found the way sops finds it: an age identity in `~/.config/sops/age/keys.txt` or `$SOPS_AGE_KEY_FILE`, a KMS, PGP. |
| `k8s://<context>/<namespace>/<secret>/<key>[#<dotted.path>]` | the embedded Kubernetes client, with that context of your kubeconfig (`$KUBECONFIG`, else `~/.kube/config`) | Read access to the Secret. The only read of your own kubeconfig the lab makes. |

A dotted path takes mappings by key and list items by index, and a key with
dots in it resolves unquoted (`stringData.credentials.json.token`). For a
`k8s://` source it walks the YAML or JSON document the key holds.

`configure` refuses to go on while a recorded source's reader is not on PATH,
naming the sources that need it, and `discover`'s report says who places each
source.

## What `up` and `platform` do

- **A key without a value is filled.** For each recorded source whose Secret
  key is missing or empty (or holds the Anthropic placeholder), agentlab reads
  the value and merge-patches it into that key under the field manager
  `agentlab-secret-source`, creating the Secret when there is none. The
  Secret's other keys stay: the GitHub App's public ids, which agentlab's
  server-side apply owns, and the placed key never touch.
- **A key that holds a value is left alone**, and its source is not read
  again: a re-run reads no vault. To rotate, delete the key (or the Secret)
  and re-run, or use `agentlab place-secret` below.
- **A source that cannot be read fails the run**: the reader missing or not
  signed in, the file not decrypting, the path reaching nothing, an empty
  value. The error names the Secret, the setting and what the reader said on
  stderr. A lab whose configuration says where a credential is never comes
  up without it — never on a placeholder.
- **The value stays in agentlab's process.** It comes over a pipe from `op`
  or `sops` (their stdin empty, so a reader that wants a password fails
  instead of waiting on your terminal) or over the Kubernetes API, into
  memory, and leaves in the request body to the lab's apiserver. It is never
  a process argument, a file, a line of output or part of an error; the run
  prints the reader and the value's length.

## Placing a credential by hand

```bash
agentlab place-secret op://<vault>/<item>/<field> <namespace>/<secret>/<key>
```

reads a reference the same way and writes it into that key of the lab's
Secret, whether or not it holds a value: the way to rotate a placed
credential, and to place one `agentlab.yaml` records no source for — the
[`github` MCP server's OAuth client](platform.md#github-as-the-person-platformgithub)
and the [GitHub sign-in](github-signin.md) App's client secret. A consumer
that reads its environment only at start (the portal, Dex) takes the new
value on its next restart; the next `agentlab platform` rolls the ones the lab
manages.

## beekeeper

Where [beekeeper](https://github.com/giantswarm/beekeeper) answers on PATH,
agentlab hands every source to its `secret copy <ref> --to-secret
kind-<clusterName>/<namespace>/<secret>/<key>` instead of reading it: beekeeper
reads the value in its own process under its own vault rules (its shared
vault's service account, age identities from the vault) and agentlab prints
its answer. The rest is the same: a held key stays, a failed placement fails
the run, and `discover` names beekeeper as the one that places each source.
