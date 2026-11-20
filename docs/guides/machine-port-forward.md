# User guide: port-forward, ssh and scp

`kaironctl port-forward`, `kaironctl ssh` and `kaironctl scp` reach a TCP
port inside a Machine without a Service, an Ingress or a route to the guest
network -- Kairon's equivalent of `virtctl port-forward`, `virtctl ssh` and
`virtctl scp`.

```console
$ export KAIRON_UI_URL=https://kairon.example:18082 KAIRON_UI_TOKEN=... KAIRON_UI_CA=ui-ca.pem
$ kaironctl port-forward web-1 8080:80
$ kaironctl ssh ubuntu@web-1
$ kaironctl scp ./app.tar ubuntu@web-1:/tmp/
```

## How it works

Each connection is a WebSocket from kaironctl to kairon-ui
(`GET /api/v1/machines/{namespace}/{name}/portforward?port=N`, with the
bearer token on the upgrade), then a WebSocket from kairon-ui to the
console relay on the Machine's node (`/portforward/{runtimeID}`), which
dials the guest. `ssh` and `scp` run the system clients with
`ProxyCommand=kaironctl port-forward --stdio %h PORT` and a
`HostKeyAlias` of `MACHINE.NAMESPACE.kairon`, so Machines with the same
name in different namespaces keep separate `known_hosts` entries.

The node relay picks the address itself, from FluxVM's record of the VM or
the addresses the guest agent reports; the caller cannot point it at any
other host.

| Network | What port-forward reaches |
|---|---|
| `user` (SLIRP, the default) | only guest ports listed in `spec.network.forwards`, through the host port FluxVM forwards |
| `tap`, `macvtap`, bridged | any port on the guest address (`status.guestIP`) |
| `netns: true` | nothing: the guest sits in its own network namespace |

## Requirements and access

- The console relay must be on (`--with-console` for `deploy-remote.sh`,
  the chart's console settings otherwise); port-forward uses the same
  node port and shared token.
- The Machine must be `Running`.
- Access follows the [console rules](machine-text-console.md): with
  `kairon.zyvor.dev/console-allowed-users` set, only those users (and
  administrators) may connect, and the legacy shared token is refused.
  With `ui.auth.namespaceScoping.enabled`, the caller needs the Machine's
  namespace.
- kairon-ui logs each session (`uiapi port-forward opened/closed`) with
  the user, Machine, port and duration.

## Limits

- TCP only.
- One WebSocket per TCP connection; there is no multiplexing.
- A guest in its own network namespace (`spec.network.netns`) is not
  reachable.
