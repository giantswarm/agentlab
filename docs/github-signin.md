# GitHub sign-in through the lab Dex

The lab's Dex knows its own users (`admin@lab.local` and the others in
`agentlab.yaml`, see [Identity](identity.md)). This page adds GitHub as a
second way in: a `github` connector on that same Dex, so a person signs in to
their lab as their GitHub self, and muster, Backstage and the apiserver trust
the token exactly as they trust a local user's — one issuer, nothing else
changes.

It is optional. A lab without it works on its local users, which is what every
proof (`platform-test`, `backstage-test`, the Playwright suite) signs in as;
what is missing without it is only the sign-in as a real GitHub account, for
a demo or for using the portal as yourself.

## What you need

- A **GitHub App** (or an OAuth App) with a client id and a client secret. Its
  permissions are the ones a sign-in reads — the person's profile, verified
  email addresses (`emails` read) and organization membership (`members`
  read) — and nothing on repositories. A sign-in App is a sign-in App: the
  lab's optional `github` MCP server ([GitHub as the person](platform.md#github-as-the-person-platformgithub))
  has a client of its own, and the two are never the same App.
- Its **callback URL** is the lab Dex's own callback, `<issuer>/callback`:
  `https://localhost:32000/dex/callback` for the default lab (`dexPort`
  32000), `https://dex.<domain>:<dexPort>/dex/callback` for a lab with its
  own certificate ([TLS](tls.md#bring-your-own-certificate)). A loopback URL
  works because GitHub only redirects the browser there. `agentlab configure`
  and the `up`/`platform`/`reload` runs print it.
- One App serves several labs: GitHub Apps take up to ten callback URLs, so
  every lab, or every Dex port, adds its own to the list. Register an App of
  your own under your account or organization, or reuse one your team shares;
  what reaches the lab is its client id and its client secret, nothing else.

## Setting it up

1. Register or reuse the App (Settings → Developer settings → GitHub Apps):
   the permissions above, the lab's callback URL in its callback list,
   *Expire user authorization tokens* on, no webhook. Generate a client
   secret.
2. Configure the lab with the App's client id. The id is public and lives in
   `agentlab.yaml`:

   ```sh
   agentlab configure --defaults --github-signin-client-id <client id>
   # optional: members of these organizations only, their teams as groups
   agentlab configure --defaults --github-signin-orgs giantswarm
   ```

   which writes

   ```yaml
   platform:
     githubSignIn:
       enabled: true
       clientId: <client id>
       orgs: [giantswarm]     # optional
   ```

3. Place the client secret in the lab as the Secret `dex/github-signin-client`
   (key `client-secret`). agentlab never reads, prints or writes the value;
   it reads which keys the Secret carries. Your secret tooling puts it there
   straight from wherever you keep it — with beekeeper, straight from the
   vault into the lab's apiserver, so the value passes through no shell, file
   or agent session:

   ```sh
   beekeeper secret copy op://<vault>/<item>/client-secret --to-secret kind-agentlab/dex/github-signin-client/client-secret
   ```

   or, from a terminal of your own, `kubectl --kubeconfig state/kubeconfig -n dex create secret generic github-signin-client --from-literal=client-secret=…`
   (the value then sits in your shell history; the broker path leaves no
   trace). The Secret's namespace is `dex`, because the Dex pod reads it as an
   environment variable and a pod reads Secrets of its own namespace only;
   `platform.githubSignIn.secret` names another Secret in that namespace.
   On a fresh lab the namespace exists after the first `agentlab up`, so
   place the Secret after it.

4. Apply: `agentlab platform` (or `agentlab reload`, which applies the Dex
   config alone). The run renders the connector into the Dex config, rolls
   the pod once and says so:

   ```
   GitHub sign-in on: connector github with client <client id>; the App's callback URL is https://localhost:32000/dex/callback
   ```

   Without the Secret the run warns, names the Secret and the key it looked
   for and the command that places it, and goes on with the local users; the
   next `platform` or `reload` renders the connector in.

5. Sign in: open the portal (`agentlab open portal`), **Sign In**; the Dex
   page now offers *Log in with Email* (the local users) and *Log in with
   GitHub*. GitHub asks the person to authorize the App once; back on Dex the
   token carries the GitHub email as `email` and the GitHub login as the
   name. Backstage's sign-in resolver takes the email's local part as the
   user entity (the connector's id is `github`, not an installation's
   `giantswarm-github`, whose users it would look up in a catalog the lab
   does not ingest GitHub into), so no catalog entity is needed.
   `agentlab login --browser` signs a kubeconfig in the same way.

## What a GitHub user is in the lab

- **Groups.** The lab's RBAC binds the three lab groups and nothing else
  ([Identity](identity.md#the-users)). A GitHub user has none of them: with
  `orgs` set, the token carries the person's teams as `<org>:<team-slug>`;
  without it, no groups. So the person reaches the portal and muster as
  themselves, and the apiserver answers *Forbidden* to their cluster calls —
  the shape an installation gives a signed-in person before rbac-operator
  binds them. For cluster rights in a demo, sign in as `admin@lab.local`.
- **`orgs`** restricts who may sign in at all: members of the listed
  organizations. Empty admits any GitHub account, which on a loopback lab
  reaches nobody but you.
- **Email.** Dex's GitHub connector needs a verified primary email on the
  GitHub account; an account without one is refused at the connector with
  that reason.

## Changing the App's permissions

A GitHub App's permission change does not reach authorizations people already
gave it: Dex's `GET /user/emails` keeps answering 403 for them until they
re-authorize. Every signed-in person revokes the App (GitHub → Settings →
Applications → Authorized GitHub Apps → revoke) and signs in again, which
asks for consent with the new permissions.

## Rotating or removing

- A rotated client secret: place the new value into the same Secret key and
  run `agentlab platform` (or `reload`); the Secret's version is part of the
  rendered pod template, so the pod rolls and reads the new value.
- A new App: `agentlab configure --defaults --github-signin-client-id <id>`
  and the new secret as above.
- Off: `agentlab configure --defaults --github-signin=false` (the client id
  stays in the file for the next time) and `agentlab platform`; the
  connector leaves the Dex config and the pod rolls. The Secret is yours and
  stays until you delete it.

## What stays in the repository and the lab

The client id (public) in `agentlab.yaml` and the rendered `state/dex.yaml`,
which carries `clientSecret: $GITHUB_SIGNIN_CLIENT_SECRET` — the variable the
Dex pod reads from the Secret, expanded by Dex at start. No secret value, no
vault name and no item name is in this repository, in `agentlab.yaml` or in
`state/`; the operator's secret tooling is the one thing that knows where the
value comes from.
