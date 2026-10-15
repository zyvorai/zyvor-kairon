# User guide: container sandboxes (`spec.sandbox.oci`)

Run an OCI container image as a Machine, in its own lightweight VM on a
FluxVM node, with a docker-like CLI. There is no Docker daemon, no Podman
and no Colima: FluxVM mounts the image's root filesystem with its own
init and runs the entrypoint. Experimental.

## Where it runs

FluxVM provides the container track on **macOS hosts**
(Virtualization.framework; `linux/amd64` images run under Rosetta). The
Linux hypervisors do not run container images, so on a mixed cluster pick
macOS nodes with a node selector. A node whose FluxVM cannot run the track
reports the error on the Machine. The host needs FluxVM's OCI boot
artifacts (`oci-kernel`, `oci-initrd`; built by FluxVM's
`scripts/build-oci-boot.sh` on Linux arm64, see FluxVM's
`docs/oci-sandboxes.md`); without them create fails with a message naming
the missing artifact.

## The CLI

```bash
# Run to completion, print the exit code, remove the Machine
kaironctl container run --wait --rm alpine:3.22 sh -c 'echo hi; exit 3'
echo $?          # 3

# A long-running service with a published port and a restart policy
kaironctl container run --name web -p 8080:80 --restart always nginx:alpine

# x86-64 image under Rosetta, pinned to Mac nodes
kaironctl container run --platform linux/amd64 --node-selector kubernetes.io/os=darwin \
  --wait alpine:3.22 uname -m

kaironctl container ps [-a]
kaironctl container inspect web
kaironctl container rm web
```

Flags go before the image, as with `docker run`; everything after the image
is the container's command. `--wait` waits for the process to exit and
returns its exit code; `--rm` implies `--wait` and deletes the Machine
afterwards; `-d` returns once the Machine exists.

## The Machine

```yaml
apiVersion: kairon.zyvor.dev/v1
kind: Machine
metadata: {name: web}
spec:
  resources: {cpu: "1", memory: 512Mi}      # optional; memory is at least 512Mi
  sandbox:
    oci:
      image: nginx:alpine
      ports: ["8080:80"]                     # host ports 1024+, TCP only
      restart: always
      healthCheck: {command: [wget, -q, -O, /dev/null, "http://127.0.0.1"], intervalSeconds: 5}
```

Fields: `image` (required), `platform`, `command`, `entrypoint`, `env`
(`KEY=value`), `workdir`, `user`, `writableRoot`, `exitPolicy` (`keep` or
`poweroff`), `ports`, `restart` (`no`, `on-failure`, `always`),
`maxRestarts`, `healthCheck`, `offline`, `allowHosts` (an egress allow-list),
`profile` (`tiny`, `small`, `standard`).

Rules checked at admission:

- It excludes `spec.image`, `spec.sandbox.templateName` and `spec.volumes`.
- `ports` cannot combine with `offline` or `allowHosts`, which have no
  network card to publish on.
- `spec.sandbox.oci` is immutable: delete and recreate the Machine to
  change the container.

`status.container` carries the process state, read from FluxVM's sandbox
logs endpoint on each reconcile: `exitCode` once it has exited, `restarts`,
`health` and `initError`.

## Not included yet

- **Secrets:** FluxVM supports write-only `secret_env`, but a Machine
  object is readable by anyone who can read Machines, so there is no field
  for it until Secret references are wired in. Do not put credentials in
  `env`.
- **Logs:** `kaironctl container logs` is not provided; the console tail
  and exit code are on FluxVM's `GET /v1/sandboxes/{id}/logs`, which Kairon
  reads only for `status.container`.
- **Volumes, exec and compose:** named volumes, `container exec` and a
  Docker Engine API socket (for `DOCKER_HOST` and `podman --url`) are
  follow-ups.
- **Linux hosts:** not supported by FluxVM's container track today.
