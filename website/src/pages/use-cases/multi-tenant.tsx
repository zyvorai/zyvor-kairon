// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

import type {ReactNode} from 'react';
import UseCase from '@site/src/components/zv/UseCase';
import {USE_CASES} from '@site/src/data/useCases';

export default function Page(): ReactNode {
  return <UseCase d={USE_CASES['multi-tenant']} />;
}
