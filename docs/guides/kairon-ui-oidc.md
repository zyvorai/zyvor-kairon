# User guide: OIDC/SSO for kairon-ui

`ui.oidc.enabled` (Helm chart, off by default) adds "Sign in with SSO" to
the dashboard's login screen -- an Authorization Code + PKCE flow against
an external OpenID Connect identity provider, alongside (not instead of)
`ui.auth.users` and the legacy shared `ui.token`.

## Why this breaks Go-stdlib-only, deliberately

This is the one place in Kairon that pulls in dependencies beyond the Go
standard library: `golang.org/x/oauth2` and `github.com/coreos/go-oidc/v3`
(which itself needs `github.com/go-jose/go-jose/v4` for JWT/JWK
handling). Every other Kairon component -- `kairon-controller`,
`kairon-node`, `kaironctl`, and `kairon-ui` itself without OIDC enabled --
stays exactly as stdlib-only as before; enabling `ui.oidc.enabled` doesn't
change what those components link against at all.

The reason this exception was made, specifically here: real JWT signature
verification against a JWK set (rotating keys, multiple algorithms, clock
skew, replay/nonce handling) is genuinely hard to get right, and hand-
rolling it would trade a well-reviewed, widely-used library for a
custom implementation nobody should trust more. That's a different
calculus than everywhere else Kairon avoids a dependency -- those are
almost always "this would be *convenient*, not *necessary*." Real OIDC
verification is closer to necessary once you've decided to support it at
all.

## Setup

Register a client with your identity provider first. You'll need:

- **Redirect URI**: `https://<your-kairon-ui-host>/api/v1/auth/oidc/callback`
  -- register this exactly; kairon-ui never guesses its own externally-
  reachable URL (proxies and ingress hostnames vary too much to infer
  safely), so `ui.oidc.redirectURL` must match what you register, exactly.
- **Client type**: confidential (with a client secret) or public (PKCE
  only, no secret) both work -- `ui.oidc.clientSecret` may be left empty
  for a public client. PKCE is always used regardless.
- **Scopes**: `openid` is required; `ui.oidc.scopes` defaults to
  `openid,profile,email`. Whatever claim `ui.oidc.usernameClaim` names
  (default `email`) must actually be returned by your provider for the
  scopes you request.

```bash
helm upgrade --install kairon ./charts/kairon -n kairon-system \
  --set ui.enabled=true \
  --set ui.oidc.enabled=true \
  --set ui.oidc.issuerURL=https://idp.example.com \
  --set ui.oidc.clientID=kairon-ui \
  --set ui.oidc.clientSecret=... \
  --set ui.oidc.redirectURL=https://kairon.example.com/api/v1/auth/oidc/callback
```

`ui.oidc.issuerURL` must serve a standard OIDC discovery document at
`{issuerURL}/.well-known/openid-configuration`. `kairon-ui` fetches this
once, at process startup -- a misconfigured or unreachable issuer fails
the container immediately (a visible `CrashLoopBackOff`), not a confusing
500 the first time someone tries to sign in.

`ui.oidc.existingSecret`/`existingSecretKey` reference a pre-created
Secret for the client secret instead, the same escape hatch every other
credential in this chart already has (`ui.existingSecret`,
`migration.tlsSecretName`, ...).

## What actually happens (the flow)

1. The browser hits `GET /api/v1/auth/oidc/login`. kairon-ui generates a
   PKCE code verifier and a nonce, signs both into the OAuth2 `state`
   parameter (HMAC-derived from `sessionSecret`, but with a distinct,
   domain-separated key -- a `state` value can never be replayed as a
   session token or vice versa), and redirects to the provider.
2. The operator authenticates with the provider (however it wants --
   password, hardware key, another SSO hop, whatever). kairon-ui never
   sees a password.
3. The provider redirects back to `GET /api/v1/auth/oidc/callback` with a
   `code` and the same `state`. kairon-ui verifies `state`'s signature and
   expiry (10 minutes), exchanges `code` for tokens (PKCE-verified), then
   verifies the returned ID token's signature, issuer, audience, expiry,
   **and nonce** against the provider's published keys.
4. `ui.oidc.usernameClaim` (default `email`) becomes the session's
   username, and kairon-ui issues a normal session token -- the exact same
   `signSession` primitive `POST /api/v1/auth/login` already uses.
   If `ui.oidc.adminGroups` (or `ui.oidc.namespaceGroups`) is set, the ID
   token's own `ui.oidc.groupsClaim` (default `groups`) is also recorded on
   the token, so admin-gated routes (`uiapi.Server.isAdminIdentity`) can re-check it against
   current config on every later request -- see "Identity and
   authorization model" below. From here on, an OIDC-authenticated
   session verifies through the exact same `withAuth` path a password
   session does either way -- `verifySession` just also returns whatever
   groups (if any) were recorded at login time.
5. kairon-ui redirects the browser to `/oidc/callback#token=...` -- a URL
   *fragment*, deliberately, never the query string: a fragment is never
   sent to any server on a subsequent request, so the token never lands in
   an access log or a `Referer` header. The SPA reads it client-side and
   stores it exactly like a password login's token (`sessionStorage`).

## Login is bound to the browser that started it

`GET /api/v1/auth/oidc/login` also sets a short-lived `HttpOnly`,
`SameSite=Lax` cookie (`Secure` over HTTPS) holding the login nonce, and the
callback only completes when that cookie matches the signed `state`. A
callback URL started in one browser and opened in another (login CSRF) is
rejected with "invalid or expired login attempt". Lax still sends the cookie on
the provider's top-level redirect back to kairon-ui, so normal sign-in is
unaffected.

## Identity and authorization model

**OIDC identities never map onto local accounts.** An OIDC session's subject
is `oidc:` plus the `ui.oidc.usernameClaim` value (for example
`oidc:alice@example.com`), so it can never equal a `ui.auth.users` entry, and
an identity provider user cannot inherit a local account's admin role or
namespaces by choosing a matching name. Audit-log entries for OIDC sessions
therefore show the `oidc:` prefix, and a person who signs in both ways appears
as two identities. When `usernameClaim` is `email`, a token whose
`email_verified` claim is explicitly `false` is rejected. (Before v0.10.3 the
raw claim was used as the session subject, and a matching local username gave
the OIDC session that account's rights. OIDC sessions issued by an older
version stop working once after upgrading; users sign in again.) Since v0.10.4 the
other direction is closed as well: a local `ui.auth.users` entry may not start with
`oidc:` (the dashboard refuses to start with one and would not log it in), because a
session subject with that prefix is always treated as a valid OIDC identity.

**OIDC admin capability is opt-in, via group mapping.** By default,
`ui.oidc.adminGroups` is empty and an OIDC-authenticated session can do
everything a normal operator can -- create/delete Machines, migrate,
snapshot -- but nothing admin-only, exactly Kairon's original OIDC
behavior. Setting `ui.oidc.adminGroups` (and, if your IdP doesn't call the
claim `"groups"`, `ui.oidc.groupsClaim`) grants admin capability to any
OIDC session whose ID token's groups claim contains one of the configured
names -- including `POST /api/v1/users/{username}/password`, resetting
another operator's password. This is checked **fresh on every request**
against the *current* `ui.oidc.adminGroups` config
(`uiapi.Server.isAdminIdentity`), not baked into the session token at
login time -- an operator changing `adminGroups` (and rolling the
deployment) takes effect for an already-signed-in caller's very next
request, without them needing to log in again. The group membership
itself, though, is only as fresh as the caller's own last login: it comes
from the ID token issued at sign-in time, not re-queried from the IdP on
every request, so a group added to someone's IdP account mid-session
won't grant admin until they sign in again.

**`POST /api/v1/auth/password` (changing your *own* password) still
doesn't apply to an OIDC session**, admin group or not -- an OIDC identity
has no `PasswordHash` in Kairon at all (`findUser` never finds an
OIDC-only username in `ui.auth.users`), so there's nothing for that route
to change regardless of admin status. Only `POST /api/v1/users/{username}/password`
(an admin resetting *someone else's* static-account password) is
affected by `adminGroups`.

**Logout works normally.** `POST /api/v1/auth/logout` revokes an
OIDC-issued session token exactly like a password-issued one -- there's
no separate "OIDC logout" concept, and no round trip back to the identity
provider (a real limitation: signing out of Kairon doesn't sign the
operator out of the identity provider itself, or any other application
using it).

## Real limits today (first cut)

- Group-to-admin mapping (`ui.oidc.adminGroups`) is a flat allowlist of
  group *names* -- no group-to-role hierarchy, no nested/derived groups,
  and no support for an IdP that only exposes group membership via a
  separate userinfo/Graph API call instead of the ID token's own claims
  (this reads only what's already in the verified ID token, deliberately,
  to avoid an extra network round trip and a second thing to verify).
- An admin-group membership change on the IdP side only takes effect at
  the caller's *next login* (the ID token is only ever read at sign-in
  time) -- see above. A change to `ui.oidc.adminGroups` on the Kairon side
  takes effect immediately, for every already-signed-in caller, no
  re-login needed -- the two have different freshness properties, don't
  confuse them.
- No provider-initiated (SP-initiated only) logout or single-logout (SLO)
  propagation back to the identity provider.
- No refresh-token use: a kairon-ui session is a fixed 12-hour token, same
  TTL as a password login, not renewed against the identity provider in
  the background. Re-authenticating after expiry means clicking "Sign in
  with SSO" again.
- `ui.oidc.usernameClaim` is a single, fixed claim name -- no fallback
  chain (e.g. "try `email`, then `preferred_username`") and no
  provider-specific claim transformation.
