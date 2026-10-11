// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

import { useEffect, useState } from 'react';
import { api } from '../api';
import { Insight, insightCounts, insightHref } from '../lib/insights';

// Deterministic attention list from GET /api/v1/insights. Renders nothing
// when there is nothing to flag, so a healthy fleet stays quiet.
export default function InsightsStrip() {
  const [items, setItems] = useState<Insight[]>([]);

  useEffect(() => {
    let live = true;
    const load = () =>
      api<Insight[]>('/api/v1/insights')
        .then((r) => live && setItems(r ?? []))
        .catch(() => live && setItems([]));
    load();
    const t = setInterval(load, 15000);
    return () => {
      live = false;
      clearInterval(t);
    };
  }, []);

  if (items.length === 0) return null;
  const c = insightCounts(items);
  return (
    <section className="insights" aria-label="Needs attention">
      <header>
        <strong>Needs attention</strong>
        <span className="usageHint">
          {c.critical > 0 && `${c.critical} critical`}
          {c.critical > 0 && c.warning > 0 && ' · '}
          {c.warning > 0 && `${c.warning} warning${c.warning === 1 ? '' : 's'}`}
        </span>
      </header>
      <ul>
        {items.slice(0, 6).map((i) => (
          <li key={i.id} className={'insight ' + i.severity}>
            <a href={insightHref(i)}>
              <span className="insight-dot" aria-hidden="true" />
              <span className="insight-body">
                <span className="insight-title">{i.title}</span>
                {i.detail && <span className="insight-detail">{i.detail}</span>}
              </span>
            </a>
          </li>
        ))}
      </ul>
      {items.length > 6 && <p className="usageHint">and {items.length - 6} more</p>}
    </section>
  );
}
