#!/bin/sh
# Copyright 2026 Zyvor · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0
#
# open-iscsi's iscsiadm CLI needs a running iscsid to actually perform a
# login -- there's no init system inside this container to start it any
# other way, and requiring the host to already run one would make this
# first cut depend on operator-specific node setup this chart can't
# verify. Self-contained by default: start our own iscsid.
#
# But not unconditionally -- confirmed against a real host that already
# ran open-iscsi natively (iscsid.socket, systemd-managed, common on
# storage-capable nodes): with hostNetwork: true, this container shares
# the node's own network namespace, and iscsid's IPC socket lives in the
# *abstract* Unix socket namespace, which is scoped to the network
# namespace, not the container. Binding our own iscsid over one already
# bound by the host fails outright ("Can not bind IPC socket"), and would
# otherwise kill this whole script before kairon-csi-node ever started.
# `iscsiadm -m iface` is a real round-trip to iscsid (not just a socket
# existence check) -- confirmed on that same host that a container-side
# iscsiadm transparently reaches the host's already-running iscsid this
# way, so skipping our own start is safe whenever this succeeds.
set -e
if ! iscsiadm -m iface >/dev/null 2>&1; then
  /sbin/iscsid
fi
exec /kairon-csi-node "$@"
