// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

import type {ReactNode} from 'react';
import Layout from '@theme/Layout';
import {
  Actions,
  Band,
  Btn,
  CopyCommand,
  CtaBand,
  Frame,
  Headline,
  List,
  Steps,
  SubNav,
  TextLink,
} from '@site/src/components/zv';

export type UseCaseData = {
  slug: string;
  eyebrow: string;
  title: ReactNode;
  seoTitle: string;
  description: string;
  lede: ReactNode;
  problem: ReactNode[];
  how: {title: string; body: ReactNode}[];
  commands: string[];
  mermaid: string;
  diagramCaption: string;
  verified: ReactNode[];
  limits: ReactNode[];
  docs: {to: string; label: string}[];
  deploy: {to: string; label: string};
};

export default function UseCase({d}: {d: UseCaseData}) {
  return (
    <Layout title={d.seoTitle} description={d.description}>
      <SubNav
        title={d.eyebrow}
        links={[
          {to: '/use-cases', label: 'All use cases'},
          {to: '#how', label: 'How'},
          {to: '#try', label: 'Try it'},
          {to: '#honest', label: 'Status'},
        ]}
      />
      <main>
        <Band tone="black" glow center>
          <Headline size="display" eyebrow={d.eyebrow} title={d.title} lede={d.lede} />
          <Actions>
            <Btn to={d.deploy.to}>{d.deploy.label}</Btn>
            <Btn to="#how" variant="ghost">
              See how
            </Btn>
          </Actions>
        </Band>

        <Band tone="light">
          <Headline eyebrow="The problem" title="Why this is hard today." />
          <List items={d.problem} />
        </Band>

        <Band tone="soft" id="how" center>
          <Headline eyebrow="How Kairon does it" title="The pieces." />
          <Steps steps={d.how} />
          <Frame mermaid={d.mermaid} caption={d.diagramCaption} />
        </Band>

        <Band tone="light" id="try">
          <Headline eyebrow="Try it" title="The commands." />
          {d.commands.map((c) => (
            <CopyCommand key={c}>{c}</CopyCommand>
          ))}
          <p>
            {d.docs.map((x, i) => (
              <span key={x.to}>
                {i > 0 && ' · '}
                <TextLink to={x.to}>{x.label}</TextLink>
              </span>
            ))}
          </p>
        </Band>

        <Band tone="soft" id="honest">
          <Headline eyebrow="Honest status" title="What is verified, and the limits." />
          <div className="zv-two">
            <div>
              <h3>Verified or documented</h3>
              <List items={d.verified} />
            </div>
            <div>
              <h3>Limits today</h3>
              <List limits items={d.limits} />
            </div>
          </div>
        </Band>

        <CtaBand title="Deploy it, then prove it." lede="Every path ends with kaironctl selftest booting a real VM.">
          <Btn to="/deploy">Deploy Kairon</Btn>
          <Btn to="/use-cases" variant="ghost">
            More use cases
          </Btn>
        </CtaBand>
      </main>
    </Layout>
  );
}
