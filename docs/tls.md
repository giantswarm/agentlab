# TLS: one lab CA, trusted explicitly

Everything the lab serves over TLS — the agentgateway edge
(`*.127.0.0.1.nip.io`) and the bundled Dex (`https://localhost:32000/dex`) —
chains to a **lab CA** that `agentlab up` mints per machine into `certs/`
(key 0600, gitignored, never leaves the machine). Every in-cluster consumer
trusts it automatically (muster's trust pool, Backstage, the apiserver's OIDC
flags). Your browser and your Node don't, until you say so:

```bash
./agentlab trust      # one sudo prompt; ./agentlab untrust reverts it
```

`trust` installs `certs/ca.crt` into the system trust store (Linux
`update-ca-certificates`/`update-ca-trust`, the macOS system keychain, the
Windows root store — the same mechanism as mkcert, via smallstep/truststore)
and, when the NSS `certutil` tool is installed, into the Firefox/Chromium NSS
profiles. Every lab URL then gets a green lock, the Dex login included.
Trust changes are always explicit: `up` only *probes* and points here, `down`
never touches a trust store, and `untrust` removes exactly the lab CA.

The CA you are trusting is deliberately narrow:

- **X.509 name constraints** pin it to the lab's own names (`platform.domain`,
  `localhost`, the Dex in-cluster names) and to `127.0.0.0/8`, so a leaked CA
  key cannot sign for the web at large. (Non-critical for old-verifier
  compatibility; Go, OpenSSL, Chrome, Firefox and macOS enforce them anyway.)
- **Leafs live 825 days** — Apple's cap: macOS rejects longer-lived TLS server
  certs *even under a user-trusted root* — and re-mint automatically from the
  unchanged CA, so a leaf rotation never repeats the trust step.
- Changing `platform.domain` **re-mints the CA** (the constraints pin the
  domain). `agentlab up`/`certs` say so loudly: run `agentlab trust` again —
  it also sweeps the replaced CA out of the stores — and recreate a running
  cluster (`agentlab down && agentlab up`), whose apiserver pinned the old CA
  at boot.

Skipping the trust step keeps the old behavior: browser warnings once per
hostname, and `export NODE_EXTRA_CA_CERTS=$PWD/certs/ca.crt` for Node
clients. The headless `*-test` commands trust `certs/ca.crt` directly and
never need any of this.

## Node and Claude Code

With the CA in the system store, Node **>= 22.15** picks it up with one env
var — the per-shell `NODE_EXTRA_CA_CERTS` export is gone:

```bash
export NODE_USE_SYSTEM_CA=1
claude mcp add --transport http muster https://muster.127.0.0.1.nip.io/mcp
```

Older Node keeps needing `NODE_EXTRA_CA_CERTS` (it ignores system stores
entirely); the `agentlab up` output prints the right line for the Node it
detects.

## Known gaps

- **Firefox without `certutil`**: Firefox reads its own NSS database, not the
  system store. `agentlab trust` covers it only when NSS tools are installed
  (`apt install libnss3-tools`, `dnf install nss-tools`, `pacman -S nss`,
  `brew install nss` — then re-run `agentlab trust`). Alternative: set
  `security.enterprise_roots.enabled` to `true` in `about:config`, which
  makes Firefox honor the system store.
- **WSL2**: the browser lives on the Windows side, which has its own trust
  store. `agentlab trust` inside WSL covers curl/Node/Claude Code there;
  import `certs/ca.crt` on the Windows side manually (an admin
  `certutil.exe -addstore root ca.crt`, or certmgr.msc) for the browser.

## Bring your own certificate

If you own a domain you can skip lab-CA trust for the edge entirely: point a
wildcard record (`*.lab.example.com` → 127.0.0.1) at loopback, mint a real
wildcard cert with whatever ACME tooling you already run (certbot, lego,
step, cert-manager — DNS-01, since a laptop lab is not publicly reachable),
and hand the pair to the lab:

```yaml
platform:
  domain: lab.example.com
  tls:
    certFile: /path/to/fullchain.pem
    keyFile: /path/to/privkey.pem
```

The edge then serves your certificate instead of a minted wildcard (renewals:
re-run `agentlab platform` after the files change). Caveat: the Dex login
page still serves the lab-CA cert — the issuer cannot move under your domain
yet ([#20](https://github.com/giantswarm/agentlab/issues/20)) — so
the login hop keeps warning until you `agentlab trust`.
