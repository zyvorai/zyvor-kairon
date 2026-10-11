// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

import {
  Activity, Archive, ArrowLeftRight, Box, Boxes, Disc3, Calendar, Camera, Cpu, Gauge, HardDrive, Layers, Network, RotateCcw, Route, Server,
  Shield, ShieldCheck, Sparkles, Stethoscope, UserCog,
} from 'lucide-react';
import type { ReactNode } from 'react';

const ICONS: Record<string, ReactNode> = {
  overview: <Activity size={18} />,
  machines: <Box size={18} />,
  machinesets: <Layers size={18} />,
  instancetypes: <Cpu size={18} />,
  nodes: <Server size={18} />,
  storage: <HardDrive size={18} />,
  pools: <Boxes size={18} />,
  images: <Disc3 size={18} />,
  backups: <Archive size={18} />,
  migrations: <ArrowLeftRight size={18} />,
  'migration-policies': <Route size={18} />,
  snapshots: <Camera size={18} />,
  'snapshot-schedules': <Calendar size={18} />,
  restores: <RotateCcw size={18} />,
  quotas: <Gauge size={18} />,
  'disruption-budgets': <Shield size={18} />,
  'network-policies': <Network size={18} />,
  'security-groups': <ShieldCheck size={18} />,
  fleet: <Layers size={18} />,
  system: <Stethoscope size={18} />,
  assistant: <Sparkles size={18} />,
  account: <UserCog size={18} />,
};

export function iconFor(id: string): ReactNode {
  return ICONS[id] ?? <Box size={18} />;
}
