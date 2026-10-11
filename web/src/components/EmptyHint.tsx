// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

import { currentNamespace } from '../lib/namespace';

// EmptyHint replaces a bare "No X yet." in a table with where the list is
// scoped (the namespace) and the first thing to do about it.
export default function EmptyHint({
  what,
  namespaced = true,
  hint,
  command,
}: {
  what: string;
  namespaced?: boolean;
  hint?: string;
  command?: string;
}) {
  return (
    <>
      No {what}
      {namespaced ? (
        <>
          {' '}in namespace <b>{currentNamespace()}</b>
        </>
      ) : null}{' '}
      yet.
      {hint && (
        <>
          <br />
          <small>{hint}</small>
        </>
      )}
      {command && (
        <>
          <br />
          <small>
            Try <code>{command}</code>
          </small>
        </>
      )}
    </>
  );
}
