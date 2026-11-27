// Run caps on the Configuration tab (issue 3137): the tier's caps show as the
// placeholder and bound what the field accepts; an empty field keeps them.
import { useMemo, useState, type ReactNode } from 'react';

import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { renderWithQueryAndTheme } from '@/test/renderWithQueryAndTheme';
import { server } from '@/test/setup';

import { metaBlockProblems, readMetaBlocks, type MetaBlocks, type TierRunCaps } from '../../lib/agentMetaBlocks';
import type { EditApplicationVersionFields } from '../../lib/useEditApplicationVersionFields';
import { RunCapsSection } from './RunCapsSection';

const BASE = '/api/v2';

beforeEach(() => {
  configureGeneratedClient({ baseUrl: BASE });
  server.use(
    http.get(`${BASE}/elitea_core/run_caps/prompt_lib/:projectId/:applicationId`, () =>
      HttpResponse.json({ tier: 'medium', caps: { max_tool_calls: 50, max_model_turns: 0, max_records_read: 1000, max_spend: 2 } }),
    ),
  );
});

afterEach(() => resetGeneratedClient());

function Harness(): ReactNode {
  const [blocks, setBlocks] = useState<MetaBlocks>(readMetaBlocks(undefined));
  const [caps, setCaps] = useState<TierRunCaps>({});
  const metaProblems = useMemo(() => metaBlockProblems(blocks, caps), [blocks, caps]);
  const versionFields = {
    fields: { metaBlocks: blocks } as unknown as EditApplicationVersionFields,
    setMetaBlock: (key: keyof MetaBlocks, block: MetaBlocks[keyof MetaBlocks]) => setBlocks((prev) => ({ ...prev, [key]: block })),
    setTierRunCaps: setCaps,
    metaProblems,
  };
  return (
    <>
      <RunCapsSection versionFields={versionFields} projectId="7" applicationId={3} />
      <output data-testid="blocks">{JSON.stringify(blocks.run_caps)}</output>
    </>
  );
}

describe('RunCapsSection', () => {
  it('names the tier and shows its caps as placeholders', async () => {
    renderWithQueryAndTheme(<Harness />);
    expect(await screen.findByText(/risk tier, medium/)).toBeInTheDocument();
    expect(screen.getByTestId('agent-meta-run-caps-max-tool-calls')).toHaveAttribute('placeholder', '50');
    expect(screen.getByTestId('agent-meta-run-caps-max-model-turns')).not.toHaveAttribute('placeholder');
  });

  it('refuses a value above the tier cap and keeps one at or under it', async () => {
    const user = userEvent.setup();
    renderWithQueryAndTheme(<Harness />);
    await screen.findByText(/risk tier, medium/);
    const field = screen.getByTestId('agent-meta-run-caps-max-tool-calls');
    await user.type(field, '51');
    expect(await screen.findByText(/At most 50/)).toBeInTheDocument();
    await user.clear(field);
    await user.type(field, '40');
    expect(screen.queryByText(/At most 50/)).not.toBeInTheDocument();
    expect(screen.getByTestId('blocks')).toHaveTextContent('{"max_tool_calls":40}');
    await user.clear(field);
    expect(screen.getByTestId('blocks')).toHaveTextContent('null');
  });

  it('keeps the decimal point while a spend is typed', async () => {
    const user = userEvent.setup();
    renderWithQueryAndTheme(<Harness />);
    await screen.findByText(/risk tier, medium/);
    const field = screen.getByTestId('agent-meta-run-caps-max-spend');
    await user.type(field, '0.5');
    expect(field).toHaveValue('0.5');
    expect(screen.getByTestId('blocks')).toHaveTextContent('{"max_spend":0.5}');
  });
});
