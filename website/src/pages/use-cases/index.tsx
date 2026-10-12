// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

import type {ReactNode} from 'react';
import Layout from '@theme/Layout';
import {Band, Btn, Cards, CtaBand, Grad, Headline, SubNav} from '@site/src/components/zv';
import {USE_CASE_LIST} from '@site/src/data/useCases';

export default function UseCases(): ReactNode {
  return (
    <Layout
      title="Use cases"
      description="What teams run on Kairon: CI and AI agent sandboxes, a KubeVirt replacement, VMware migration, private AI on Macs and GPUs, multi-tenant isolation, and edge or lab hosts.">
      <SubNav title="Use cases" links={[{to: '/compare', label: 'Compare'}, {to: '/features', label: 'Features'}]} />
      <main>
        <Band tone="black" glow center>
          <Headline
            size="display"
            eyebrow="Use cases"
            title={
              <>
                One platform, <Grad>many shapes.</Grad>
              </>
            }
            lede="Pick the story closest to yours. Each one says how Kairon does it, the commands, and what is and is not verified."
          />
        </Band>
        <Band tone="light" center>
          <Cards
            cards={USE_CASE_LIST.map((u) => ({
              eyebrow: u.eyebrow,
              title: u.seoTitle,
              body: u.lede,
              to: `/use-cases/${u.slug}`,
            }))}
          />
        </Band>
        <CtaBand title="Not on the list?" lede="Kairon is a general VM platform; these are the shapes we have documented so far.">
          <Btn to="/deploy">Deploy Kairon</Btn>
          <Btn to="/docs/WHAT_SHIPS" variant="ghost">
            Everything that ships
          </Btn>
        </CtaBand>
      </main>
    </Layout>
  );
}
