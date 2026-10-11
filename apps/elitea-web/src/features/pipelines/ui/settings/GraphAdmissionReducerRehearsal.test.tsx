/**
 * Typed state reducers on a rehearsal build (`VITE_GRAPH_EXTENSIONS_REHEARSAL=true`), where the Worker admits
 * them: a valid reducer is a non-blocking, YAML-only notice, and every compiler refusal of a reducer is mirrored.
 * The production refusal is covered in `graphAdmission.helpers.test.ts` and `GraphAdmissionGate.test.tsx`.
 */
import type { ReactNode } from 'react';
import { FormProvider, useForm } from 'react-hook-form';

import { cleanup, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import type { YamlPipelineDocument } from '../../lib/flow-editor/helpers/pipelineFlow.types';

afterEach(() => {
  cleanup();
  vi.unstubAllEnvs();
  vi.resetModules();
});

async function load() {
  vi.resetModules();
  vi.stubEnv('VITE_GRAPH_EXTENSIONS_REHEARSAL', 'true');
  const admission = await import('../../lib/graphAdmission.helpers');
  const live = await import('../../lib/livePipelineGraphAdmission');
  const gate = await import('./GraphAdmissionGate');
  const store = await import('../../model/pipelineYamlStore');
  const dump = await import('../../lib/dumpYaml.helpers');
  return { admission, live, gate, store, dump };
}

const BASE = {
  state: { input: 'str', messages: 'list', summary: 'str' } as Record<string, unknown>,
  entry_point: 'LLM_1',
  nodes: [{ id: 'LLM_1', type: 'llm', input: ['messages'], output: ['summary'], transition: 'END' }],
};

/** Deliberately loose: the refusal cases carry shapes the YAML type does not allow (`reducer: 5`). */
function withState(extra: Record<string, unknown>): YamlPipelineDocument {
  return { ...BASE, state: { ...BASE.state, ...extra } } as YamlPipelineDocument;
}

describe('typed state reducers on a rehearsal build', () => {
  it('turns every valid reducer into a non-blocking notice', async () => {
    const { admission } = await load();
    const issues = admission.collectGraphAdmissionIssues(
      withState({
        findings: { type: 'list', value: [], reducer: 'append' },
        total: { type: 'int', value: 0, reducer: 'sum_int' },
        seen: { type: 'dict', value: {}, reducer: 'merge' },
        plain: { type: 'str', reducer: 'overwrite' },
      }),
    );

    expect(issues.map((issue) => [issue.field, issue.subject, issue.severity])).toEqual([
      ['state.findings', 'append', 'warning'],
      ['state.total', 'sum_int', 'warning'],
      ['state.seen', 'merge', 'warning'],
      ['state.plain', 'overwrite', 'warning'],
    ]);
    expect(issues.every((issue) => issue.message.includes('YAML only'))).toBe(true);
    expect(admission.blockingIssues(issues)).toEqual([]);
  });

  it('mirrors each compiler refusal of a reducer', async () => {
    const { admission } = await load();
    const cases: readonly (readonly [Record<string, unknown>, string, string])[] = [
      [{ input: { type: 'str', reducer: 'overwrite' } }, 'compiler.rs:2789', 'cannot declare a reducer'],
      [{ findings: { type: 'list', reducer: 'extend' } }, 'compiler.rs:2794', 'must be one of'],
      [{ findings: { type: 'dict', reducer: 'append' } }, 'compiler.rs:2800', 'needs a "list" variable'],
      [{ total: { type: 'number', reducer: 'merge' } }, 'compiler.rs:2800', 'needs a "dict" variable'],
      [{ findings: { type: 'list', reducer: 5 } }, 'compiler.rs:2729', 'must be one of'],
    ];
    for (const [state, citation, message] of cases) {
      const blocking = admission.blockingIssues(admission.collectGraphAdmissionIssues(withState(state)));
      expect(blocking.map((issue) => issue.rule), citation).toEqual(['state.reducer']);
      expect(blocking[0]?.citation).toBe(citation);
      expect(blocking[0]?.message).toContain(message);
    }
  });

  it('keeps a reducer-only document admissible for the save path', async () => {
    const { live } = await load();
    const verdict = live.judgeLivePipelineGraph(
      ['state:', '  input: str', '  messages: list', '  summary: str', '  findings: {type: list, value: [], reducer: append}', 'entry_point: LLM_1', 'nodes:', '  - {id: LLM_1, type: llm, input: [messages], output: [summary], transition: END}', ''].join('\n'),
    );

    expect(verdict.isAdmissible).toBe(true);
    expect(verdict.issues.map((issue) => [issue.rule, issue.severity])).toEqual([['state.reducer', 'warning']]);
  });

  it('keeps Save available and shows the YAML-only notice in the editor', async () => {
    const { gate, store, dump } = await load();
    const document = withState({ findings: { type: 'list', value: [], reducer: 'append' } });
    store.usePipelineYamlStore.setState({ yamlCode: dump.dumpYaml(document), yamlJsonObject: document });
    function Probe(): ReactNode {
      const form = useForm({ mode: 'onChange', defaultValues: {} });
      return (
        <FormProvider {...form}>
          <span data-testid="can-save">{String(form.formState.isValid)}</span>
          <gate.GraphAdmissionGate />
        </FormProvider>
      );
    }
    const { getByTestId, queryByTestId } = renderWithTheme(<Probe />);

    await waitFor(() => expect(getByTestId('can-save')).toHaveTextContent('true'));
    expect(queryByTestId('graph-admission-gate')).not.toBeInTheDocument();
    expect(getByTestId('graph-admission-notice')).toHaveTextContent('state.findings: the "append" reducer can be changed in YAML only for now.');
  });
});
