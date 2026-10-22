# FluxVM Studio through the node relay

Every Kairon node runs a FluxVM daemon on loopback. FluxVM Studio, the daemon's web UI, is reachable through the node's
console relay (`kairon-node --console-addr`), so you do not need a port-forward:

    https://<node>:<console-port>/studio/console/

Sign in with the relay's shared bearer token (`KAIRON_NODE_CONSOLE_TOKEN`), not a FluxVM token. The relay swaps in the
daemon's own token for each API call.

| Path | Auth | Goes to |
|---|---|---|
| `/studio/console/...` | none | FluxVM `/console/...`: the UI's static files and single-use WebSocket tickets |
| `/studio/v1/...` | relay bearer | FluxVM `/v1/...`, including server-sent events; FluxVM's token replaces yours upstream |

The static files hold no data. A WebSocket ticket is minted by an authenticated `POST /studio/v1/vms/{id}/ws-ticket`
and is the only credential FluxVM checks on the upgrade, so the relay adds none.

## Security

Whoever holds the relay token already has exec, files and console on every VM on the node; through `/studio/v1` they also
get the daemon's full API with FluxVM's token. That is the same blast radius, not a new one, but it is why the relay port
must stay restricted (`scripts/cluster/restrict-kairon-relay.sh` in Veyron limits it to loopback, the node and the pod
range). Reach it over a VPN or an SSH tunnel; never publish it.
