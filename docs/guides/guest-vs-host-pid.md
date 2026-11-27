# Guest vs host PID

When the host only sees a vhost thread, the guest agent can supply the
guest PID. Rules:

- Set the scope (guest or host) on every PID field.
- Never overwrite a guest PID with a host PID.
- If the agent cannot resolve a guest PID, leave the field empty and
  set scope to host.

This is the contract for the optional Attributor in `internal/fluxvm`.
