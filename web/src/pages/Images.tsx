// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

import { useEffect, useRef, useState } from 'react';
import { api, apiErrorText, authHeaders, isAdmin } from '../api';
import { formatBytes } from '../lib/phase';
import ResourceTable from '../components/ResourceTable';
import { EmptyState } from '../components/ui';
import { Field } from '../components/JsonTerminal';
import EmptyHint from '../components/EmptyHint';

interface UploadedImage {
  name: string;
  digest: string;
  sizeBytes: number;
  format?: string;
  url: string;
  uploadedAt: string;
  uploadedBy?: string;
}

const FORMATS = ['qcow2', 'raw', 'ova', 'vmdk', 'vhd', 'vhdx'];

// Images is the dashboard face of kairon-ui's content-addressed image store
// (internal/uiapi/images.go): list, upload (admin) and delete (admin). Nodes
// pull uploaded images by digest through spec.image.source.httpURL.
export default function Images() {
  const [items, setItems] = useState<UploadedImage[]>([]);
  const [disabled, setDisabled] = useState(false);
  const [msg, setMsg] = useState('');
  const [busy, setBusy] = useState(false);
  const [name, setName] = useState('');
  const [format, setFormat] = useState('qcow2');
  const [replace, setReplace] = useState(false);
  const file = useRef<HTMLInputElement>(null);
  const admin = isAdmin();

  const refresh = () =>
    api<UploadedImage[]>('/api/v1/images')
      .then((r) => setItems(r ?? []))
      .catch((e) => {
        const t = apiErrorText(e);
        if (t.includes('not enabled')) setDisabled(true);
        else setMsg(t);
      });
  useEffect(() => {
    refresh();
  }, []);

  async function upload() {
    const f = file.current?.files?.[0];
    if (!f || !name) return;
    setBusy(true);
    setMsg('');
    try {
      const r = await fetch(`/api/v1/images/${encodeURIComponent(name)}?format=${format}${replace ? '&replace=true' : ''}`, {
        method: 'PUT',
        headers: authHeaders(),
        body: f,
      });
      if (!r.ok) throw new Error((await r.text()) || r.statusText);
      setName('');
      if (file.current) file.current.value = '';
      await refresh();
    } catch (e) {
      setMsg(apiErrorText(e));
    } finally {
      setBusy(false);
    }
  }

  async function remove(n: string) {
    if (!confirm(`Delete image "${n}"? Nodes keep their cached copies.`)) return;
    try {
      await api(`/api/v1/images/${encodeURIComponent(n)}`, { method: 'DELETE' });
      refresh();
    } catch (e) {
      setMsg(apiErrorText(e));
    }
  }

  if (disabled) {
    return (
      <EmptyState title="The image store is not enabled">
        Enable it with <code>ui.imageStore.enabled=true</code> (Helm) or <code>--image-store-dir</code> on kairon-ui to upload and serve VM images.
      </EmptyState>
    );
  }

  return (
    <div className="grid">
      <div className="card span4">
        <span className="eyebrow">IMAGES</span>
        {msg && <p className="msg error">{msg}</p>}
        {admin && (
          <div className="opform">
            <Field label="Name">
              <input value={name} onChange={(e) => setName(e.target.value)} placeholder="ubuntu-24-04" />
            </Field>
            <Field label="Format">
              <select value={format} onChange={(e) => setFormat(e.target.value)}>
                {FORMATS.map((f) => (
                  <option key={f}>{f}</option>
                ))}
              </select>
            </Field>
            <Field label="File">
              <input type="file" ref={file} />
            </Field>
            <label className="login-remember">
              <input type="checkbox" checked={replace} onChange={(e) => setReplace(e.target.checked)} />
              <span>Replace if exists</span>
            </label>
            <button className="primary" disabled={busy || !name} onClick={upload}>
              {busy ? 'Uploading…' : 'Upload'}
            </button>
          </div>
        )}
        <ResourceTable
          items={items}
          keyFn={(i) => i.name}
          emptyText={<EmptyHint what="uploaded images" namespaced={false} command="kaironctl image upload FILE --name NAME" />}
          columns={[
            { header: 'Name', render: (i) => i.name },
            { header: 'Format', render: (i) => i.format || '-' },
            { header: 'Size', render: (i) => formatBytes(i.sizeBytes) },
            { header: 'Digest', render: (i) => <code>{i.digest.slice(0, 19)}…</code> },
            { header: 'Uploaded', render: (i) => `${new Date(i.uploadedAt).toLocaleString()}${i.uploadedBy ? ' · ' + i.uploadedBy : ''}` },
            {
              header: '',
              render: (i) =>
                admin ? (
                  <button className="sm danger" onClick={() => remove(i.name)}>
                    Delete
                  </button>
                ) : null,
            },
          ]}
        />
      </div>
    </div>
  );
}
