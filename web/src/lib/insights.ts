// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

import { formatHash } from './route';
import { isPage, Page } from './nav';

export interface Insight {
  id: string;
  severity: 'critical' | 'warning';
  title: string;
  detail?: string;
  page: string;
  target?: string;
}

// insightHref links a finding to the page and (when present) inspector
// target it concerns; unknown pages fall back to the overview.
export function insightHref(i: Insight): string {
  const page: Page = isPage(i.page) ? i.page : 'overview';
  return formatHash({ page, target: i.target });
}

export function insightCounts(list: Insight[]): { critical: number; warning: number } {
  return {
    critical: list.filter((i) => i.severity === 'critical').length,
    warning: list.filter((i) => i.severity === 'warning').length,
  };
}
