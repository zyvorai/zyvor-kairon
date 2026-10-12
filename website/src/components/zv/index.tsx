// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

import {useEffect, useRef, useState} from 'react';
import type {ReactNode} from 'react';
import clsx from 'clsx';
import Link from '@docusaurus/Link';
import useBrokenLinks from '@docusaurus/useBrokenLinks';
import Mermaid from '@theme/Mermaid';
import Reveal from '@site/src/components/Reveal';

/* ------------------------------------------------------------------ */
/* Layout                                                              */
/* ------------------------------------------------------------------ */

type BandProps = {
  tone?: 'light' | 'soft' | 'black';
  glow?: boolean;
  center?: boolean;
  id?: string;
  children: ReactNode;
  className?: string;
};

export function Band({tone = 'light', glow, center, id, children, className}: BandProps) {
  // Tell Docusaurus's anchor checker that this section is a valid link target.
  const brokenLinks = useBrokenLinks();
  if (id) brokenLinks.collectAnchor(id);
  return (
    <section
      id={id}
      className={clsx('zv-band', `zv-band--${tone}`, glow && 'zv-band--glow', className)}>
      <div className={clsx('zv-inner', center ? 'zv-center' : 'zv-left')}>{children}</div>
    </section>
  );
}

type HeadlineProps = {
  eyebrow?: string;
  /** Plain text before and after the highlighted words. */
  title: ReactNode;
  lede?: ReactNode;
  size?: 'display' | 'title';
  children?: ReactNode;
};

export function Headline({eyebrow, title, lede, size = 'title', children}: HeadlineProps) {
  const Tag = size === 'display' ? 'h1' : 'h2';
  return (
    <Reveal>
      {eyebrow && <p className="zv-eyebrow">{eyebrow}</p>}
      <Tag className={size === 'display' ? 'zv-display' : 'zv-title'}>{title}</Tag>
      {lede && <p className="zv-lede">{lede}</p>}
      {children}
    </Reveal>
  );
}

export function Grad({children}: {children: ReactNode}) {
  return <span className="zv-grad">{children}</span>;
}

export function Actions({children}: {children: ReactNode}) {
  return <div className="zv-actions">{children}</div>;
}

type BtnProps = {to: string; children: ReactNode; variant?: 'primary' | 'ghost'};
export function Btn({to, children, variant = 'primary'}: BtnProps) {
  return (
    <Link className={clsx('zv-btn', `zv-btn--${variant}`)} to={to}>
      {children}
    </Link>
  );
}

export function TextLink({to, children}: {to: string; children: ReactNode}) {
  return (
    <Link className="zv-link" to={to}>
      {children}
    </Link>
  );
}

export function Pill({children, ok}: {children: ReactNode; ok?: boolean}) {
  return <span className={clsx('zv-pill', ok && 'zv-pill--ok')}>{children}</span>;
}

/* ------------------------------------------------------------------ */
/* Sticky sub-navigation                                               */
/* ------------------------------------------------------------------ */

export function SubNav({title, links}: {title: string; links: {to: string; label: string}[]}) {
  return (
    <nav className="zv-subnav" aria-label={`${title} sections`}>
      <div className="zv-subnav__in">
        <span className="zv-subnav__title">{title}</span>
        <div className="zv-subnav__links">
          {links.map((l) => (
            <Link key={l.to} to={l.to}>
              {l.label}
            </Link>
          ))}
          <Link className="zv-subnav__cta" to="/deploy">
            Deploy
          </Link>
        </div>
      </div>
    </nav>
  );
}

/* ------------------------------------------------------------------ */
/* Stats with count-up                                                 */
/* ------------------------------------------------------------------ */

export type Stat = {
  value: number;
  /** Digits after the decimal point. */
  decimals?: number;
  suffix?: string;
  label: string;
  note: ReactNode;
};

function CountUp({value, decimals = 0, suffix = ''}: Pick<Stat, 'value' | 'decimals' | 'suffix'>) {
  const ref = useRef<HTMLSpanElement>(null);
  const [shown, setShown] = useState(value);

  useEffect(() => {
    const node = ref.current;
    const reduce =
      typeof window !== 'undefined' &&
      window.matchMedia &&
      window.matchMedia('(prefers-reduced-motion: reduce)').matches;
    if (!node || reduce || typeof IntersectionObserver === 'undefined') return;
    let raf = 0;
    const io = new IntersectionObserver(
      ([entry]) => {
        if (!entry.isIntersecting) return;
        io.disconnect();
        const start = performance.now();
        const dur = 1100;
        const tick = (now: number) => {
          const t = Math.min(1, (now - start) / dur);
          const eased = 1 - Math.pow(1 - t, 3);
          setShown(value * eased);
          if (t < 1) raf = requestAnimationFrame(tick);
        };
        setShown(0);
        raf = requestAnimationFrame(tick);
      },
      {threshold: 0.6},
    );
    io.observe(node);
    return () => {
      io.disconnect();
      cancelAnimationFrame(raf);
    };
  }, [value]);

  return (
    <span ref={ref}>
      {shown.toFixed(decimals)}
      {suffix}
    </span>
  );
}

export function StatBar({stats}: {stats: Stat[]}) {
  return (
    <div className="zv-stats">
      {stats.map((s, i) => (
        <Reveal key={s.label} delay={i * 90}>
          <div className="zv-stat__num zv-grad">
            <CountUp value={s.value} decimals={s.decimals} suffix={s.suffix} />
          </div>
          <p className="zv-stat__label">{s.label}</p>
          <p className="zv-stat__note">{s.note}</p>
        </Reveal>
      ))}
    </div>
  );
}

/* ------------------------------------------------------------------ */
/* Bento                                                               */
/* ------------------------------------------------------------------ */

export type Tile = {
  title: string;
  body: ReactNode;
  to: string;
  glyph?: string;
  experimental?: boolean;
  variant?: 'default' | 'wide' | 'dark' | 'blue';
  more?: string;
};

export function Bento({tiles}: {tiles: Tile[]}) {
  return (
    <div className="zv-bento">
      {tiles.map((t, i) => (
        <Reveal key={t.title} delay={(i % 3) * 70} className={clsx('zv-cell', t.variant === 'wide' && 'zv-cell--wide')}>
          <Link
            to={t.to}
            className={clsx(
              'zv-tile',
              t.variant === 'dark' && 'zv-tile--dark',
              t.variant === 'blue' && 'zv-tile--blue',
            )}>
            {t.glyph && <span className="zv-tile__glyph" aria-hidden="true">{t.glyph}</span>}
            <h3>
              {t.title}
              {t.experimental && <span className="zv-pill">Experimental</span>}
            </h3>
            <p>{t.body}</p>
            <span className="zv-tile__more">{t.more ?? 'Learn more ›'}</span>
          </Link>
        </Reveal>
      ))}
    </div>
  );
}

/* ------------------------------------------------------------------ */
/* Terminal and copyable commands                                      */
/* ------------------------------------------------------------------ */

export type TermLine = {text: string; kind?: 'cmd' | 'ok' | 'warn' | 'dim' | 'info' | 'out'};

export function Terminal({title, lines, speed = 0.35}: {title: string; lines: TermLine[]; speed?: number}) {
  return (
    <div className="zv-term" role="img" aria-label={`Terminal: ${title}`}>
      <div className="zv-term__bar" aria-hidden="true">
        <span className="zv-term__dot" style={{background: '#ff5f57'}} />
        <span className="zv-term__dot" style={{background: '#febc2e'}} />
        <span className="zv-term__dot" style={{background: '#28c840'}} />
        <span className="zv-term__title">{title}</span>
      </div>
      <pre className="zv-term__body" aria-hidden="true">
        {lines.map((l, i) => (
          <span
            key={i}
            className={clsx('zv-term__line', l.kind && l.kind !== 'out' && `zv-term__line--${l.kind}`)}
            style={{animationDelay: `${0.4 + i * speed}s`}}>
            {l.text}
          </span>
        ))}
      </pre>
    </div>
  );
}

export function CopyCommand({children, label}: {children: string; label?: string}) {
  const [done, setDone] = useState(false);
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(children);
      setDone(true);
      setTimeout(() => setDone(false), 1600);
    } catch {
      /* clipboard unavailable: the text is selectable */
    }
  };
  return (
    <div className="zv-copy">
      <pre>
        <code>{children}</code>
      </pre>
      <button type="button" onClick={copy} aria-label={label ?? 'Copy command'}>
        {done ? 'Copied' : 'Copy'}
      </button>
    </div>
  );
}

/* ------------------------------------------------------------------ */
/* Steps, frames, cards                                                */
/* ------------------------------------------------------------------ */

export function Steps({steps}: {steps: {title: string; body: ReactNode}[]}) {
  return (
    <div className="zv-steps">
      {steps.map((s, i) => (
        <Reveal key={s.title} delay={i * 90}>
          <div className="zv-step">
            <h3>{s.title}</h3>
            <p>{s.body}</p>
          </div>
        </Reveal>
      ))}
    </div>
  );
}

type FrameProps = {
  src?: string;
  darkSrc?: string;
  alt?: string;
  caption?: ReactNode;
  mermaid?: string;
  width?: number;
  height?: number;
};

export function Frame({src, darkSrc, alt = '', caption, mermaid, width, height}: FrameProps) {
  return (
    <Reveal>
      <figure className="zv-frame">
        {mermaid ? (
          <Mermaid value={mermaid} />
        ) : (
          <picture>
            {darkSrc && <source media="(prefers-color-scheme: dark)" srcSet={darkSrc} />}
            <img src={src} alt={alt} width={width} height={height} loading="lazy" decoding="async" />
          </picture>
        )}
        {caption && <figcaption>{caption}</figcaption>}
      </figure>
    </Reveal>
  );
}

export type CardData = {
  title: string;
  body: ReactNode;
  to: string;
  eyebrow?: string;
  more?: string;
};

export function Cards({cards}: {cards: CardData[]}) {
  return (
    <div className="zv-cards">
      {cards.map((c, i) => (
        <Reveal key={c.title} delay={(i % 3) * 80}>
          <Link to={c.to} className="zv-card">
            {c.eyebrow && <span className="zv-eyebrow">{c.eyebrow}</span>}
            <h3>{c.title}</h3>
            <p>{c.body}</p>
            <span className="zv-card__more">{c.more ?? 'Read the story ›'}</span>
          </Link>
        </Reveal>
      ))}
    </div>
  );
}

export function CtaBand({title, lede, children}: {title: ReactNode; lede?: ReactNode; children: ReactNode}) {
  return (
    <Band tone="black" glow center>
      <Headline title={title} lede={lede} />
      <Actions>{children}</Actions>
    </Band>
  );
}

export function List({items, limits}: {items: ReactNode[]; limits?: boolean}) {
  return (
    <ul className={clsx('zv-list', limits && 'zv-list--limits')}>
      {items.map((it, i) => (
        <li key={i}>{it}</li>
      ))}
    </ul>
  );
}
