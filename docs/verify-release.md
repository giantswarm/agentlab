# Verifying a downloaded release

Every release asset `agentlab-<os>-<arch>` (`.exe` on Windows) ships next to its
Sigstore bundle `agentlab-<os>-<arch>.bundle`. No checksums file is published: a
checksum adds no trust beyond GitHub's own asset digest, while the bundle ties the
binary to a CircleCI build of this repository, the same check `agentlab
self-update` runs before it installs anything. Release candidates are signed by
the same identity, so the steps below verify them too.

Download the binary and its bundle into one empty directory, then run
[cosign](https://docs.sigstore.dev/cosign/system_config/installation/) (v2 or
newer) from outside it. Never run the binary before it verified:

```bash
mkdir dl && gh release download v0.96.0 --repo giantswarm/agentlab \
  -D dl -p 'agentlab-linux-amd64' -p 'agentlab-linux-amd64.bundle'

cosign verify-blob \
  --bundle dl/agentlab-linux-amd64.bundle \
  --certificate-oidc-issuer https://oidc.circleci.com \
  --certificate-identity-regexp '^https://circleci\.com/api/v2/projects/[a-f0-9-]+/pipeline-definitions/[a-f0-9-]+$' \
  dl/agentlab-linux-amd64
```

A verified binary prints:

```text
Verified OK
```

The builds are signed by CircleCI, not by a GitHub workflow: the certificate's
identity is a CircleCI pipeline definition and its issuer is
`https://oidc.circleci.com`. Its GitHub workflow repository extension is empty, so
`--certificate-github-workflow-repository` fails on these builds, and no cosign
flag pins the source repository. Check it from the bundle: the certificate's
source repository URI extension (OID `1.3.6.1.4.1.57264.1.12`) must be
`github.com/giantswarm/agentlab`.

```bash
jq -r .verificationMaterial.certificate.rawBytes dl/agentlab-linux-amd64.bundle \
  | base64 -d | openssl x509 -inform der -noout -text \
  | grep -A1 '1.3.6.1.4.1.57264.1.12'
```

```text
            1.3.6.1.4.1.57264.1.12:
                ..github.com/giantswarm/agentlab
```

A verification that fails exits non-zero and prints `Error:` with the reason; the
binary is not to be used. Pinning another identity (here a GitHub one) fails like
this, and the message names the identity the bundle carries:

```text
Error: failed to verify certificate identity: no matching CertificateIdentity found, last error: expected SAN value to match regex "^https://github\.com/giantswarm/", got "https://circleci.com/api/v2/projects/7bdef2bf-7d98-4aca-bf0c-5d3b72cc306b/pipeline-definitions/48087dfd-3bbc-5498-9a9a-f7f1bb77c43d"
```

A binary that does not match its bundle, or a bundle of another repository's
build, fails the same way, either at the identity or with a signature error; a
bundle whose source repository is not `github.com/giantswarm/agentlab` fails the
check above.
