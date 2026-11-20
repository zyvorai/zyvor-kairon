# User guide: image catalog (`spec.image.catalogName`)

Reference a named, checksummed (and optionally signed) image alias
instead of a raw disk path -- FluxVM's own image catalog, node-scoped
admin API, first cut.

## What this is

```yaml
apiVersion: kairon.zyvor.dev/v1
kind: Machine
metadata: {name: db}
spec:
  image: {catalogName: ubuntu-24.04}   # instead of image.path
  resources: {cpu: "2", memory: 2Gi}
```

`spec.image.catalogName` is passed straight through as FluxVM's own
`CreateVmRequest.image` field, which already accepts a catalog alias
interchangeably with a raw path -- kairon-node does no resolution of its
own (unlike [`spec.image.source`](machine-image-import.md), which
downloads into Kairon's own node-local cache). It's exempt from
`--image-root` path fencing entirely, since it was never a filesystem
path to begin with -- FluxVM's own catalog integrity checks (mandatory
SHA-256, optional Ed25519 signature) are the trust boundary here instead.

## Managing the catalog

Every operation is a node-scoped admin REST endpoint -- catalog entries,
like sandbox templates, are **per-node** state; registering one on
`worker-1` doesn't make it visible from `worker-2`.

- **`GET /api/v1/nodes/{node}/catalog`** -- list every entry (any
  authenticated operator, read-only).
- **`POST /api/v1/nodes/{node}/catalog`** (admin-only, body
  `{"name", "source", "format"}`) -- register a new entry. `source` is a
  local path or `http(s)://` URL; FluxVM downloads it and computes its
  own SHA-256 (unlike `spec.image.source`'s image-import cache, there's
  no way to pre-supply a digest here -- FluxVM always derives it itself).
  `format` defaults to `qcow2`.
- **`DELETE /api/v1/nodes/{node}/catalog/{name}`** (admin-only) -- remove
  an entry. Refused by FluxVM itself if the entry is marked read-only.
- **`POST .../catalog/{name}/rename`** (body `{"newName"}`) -- rename in
  place.
- **`POST .../catalog/{name}/clone`** (body `{"targetName"}`) -- copy an
  entry's underlying image under a new name, e.g. to branch off a
  read-only base image without touching it.
- **`POST .../catalog/{name}/export`** (body `{"path"}`) -- copy an
  entry's image out to an explicit node-local path.
- **`POST .../catalog/{name}/read-only`** (body `{"readOnly"}`) -- toggle
  the read-only flag that protects a base image other entries clone from.
- **`POST .../catalog/clean`** -- remove orphaned download artifacts left
  behind by a previous `POST .../catalog` call.

## Real limits today (first cut)

- **`kaironctl` has no dedicated verbs.** The only dashboard surface is the
  "Image catalog" tab in the Nodes page's node tools (register, rename,
  clone, export, read-only toggle, delete, clean); everything else goes
  through the API.
- **No signature generation from Kairon.** FluxVM's own `fluxvm catalog
  sign` CLI (out of band, on the node itself) is how a signed entry gets
  its signature -- Kairon only ever relays what FluxVM already reports,
  never creates or verifies one itself.
- **Registering an entry is a real, potentially slow operation** (a real
  image download plus a SHA-256 pass over it) -- the admin API's own
  timeout is generous (15 minutes) to match, but a very large image over
  a slow link can still exceed it.
- **No admission-time validation of `catalogName`** -- an unresolvable
  alias just fails clearly at Machine creation time (FluxVM's own error,
  surfaced unmodified), not rejected earlier by the webhook.
