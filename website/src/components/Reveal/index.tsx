// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

import {useEffect, useRef, useState} from 'react';
import type {ReactNode} from 'react';
import clsx from 'clsx';

type RevealProps = {
  children: ReactNode;
  className?: string;
  /** Stagger delay in ms, for revealing a sequence of siblings. */
  delay?: number;
};

/**
 * Fades/slides a section in once it scrolls into view. Falls back to
 * always-visible (no animation) when IntersectionObserver isn't available
 * (SSR/build) or the user prefers reduced motion — handled in CSS via
 * `.zv-reveal` (see src/css/custom.css).
 */
export default function Reveal({children, className, delay = 0}: RevealProps) {
  const ref = useRef<HTMLDivElement>(null);
  const [visible, setVisible] = useState(false);

  useEffect(() => {
    const node = ref.current;
    if (!node || typeof IntersectionObserver === 'undefined') {
      setVisible(true);
      return;
    }
    const observer = new IntersectionObserver(
      ([entry]) => {
        if (entry.isIntersecting) {
          setVisible(true);
          observer.disconnect();
        }
      },
      {threshold: 0.15},
    );
    observer.observe(node);
    return () => observer.disconnect();
  }, []);

  return (
    <div
      ref={ref}
      className={clsx('zv-reveal', visible && 'zv-reveal--visible', className)}
      style={delay ? {transitionDelay: `${delay}ms`} : undefined}>
      {children}
    </div>
  );
}
