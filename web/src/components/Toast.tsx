// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

import { AlertCircle, CheckCircle2 } from 'lucide-react';
import { createContext, ReactNode, useCallback, useContext, useState } from 'react';

type Kind = 'ok' | 'err';
interface T { id: number; msg: string; kind: Kind; undo?: () => void }
// undo, when given, adds an Undo button and keeps the toast up longer.
const Ctx = createContext<(msg: string, kind?: Kind, undo?: () => void) => void>(() => {});

export function useToast() {
  return useContext(Ctx);
}

export function ToastProvider({ children }: { children: ReactNode }) {
  const [items, setItems] = useState<T[]>([]);
  const push = useCallback((msg: string, kind: Kind = 'ok', undo?: () => void) => {
    const id = Date.now() + Math.random();
    setItems((x) => [...x, { id, msg, kind, undo }]);
    setTimeout(() => setItems((x) => x.filter((t) => t.id !== id)), kind === 'err' || undo ? 6000 : 3200);
  }, []);
  const dismiss = (id: number) => setItems((x) => x.filter((t) => t.id !== id));
  return (
    <Ctx.Provider value={push}>
      {children}
      <div className="toasts" role="status" aria-live="polite">
        {items.map((t) => (
          <div key={t.id} className={'toast ' + t.kind}>
            {t.kind === 'err' ? <AlertCircle size={16} /> : <CheckCircle2 size={16} />}
            <span>{t.msg}</span>
            {t.undo && (
              <button
                className="sm"
                onClick={() => {
                  dismiss(t.id);
                  t.undo?.();
                }}
              >
                Undo
              </button>
            )}
          </div>
        ))}
      </div>
    </Ctx.Provider>
  );
}
