/**
 * The save gate, exercised through a REAL react-hook-form instance and the
 * REAL pipeline yaml store — no module mocks (`elitea/no-vi-mock`). What is
 * asserted is the flag the pipeline editor's Save button is actually wired
 * to (`pages/pipelines/EditPipeline.tsx`: `canSave={form.formState.isValid
 * && !isSaving}`), not an internal of this component.
 */
import type { ReactNode } from 'react';
import { useState } from 'react';
import { FormProvider, useForm } from 'react-hook-form';

import { act, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { beforeEach, describe, expect, it } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { dumpYaml } from '../../lib/dumpYaml.helpers';
import type { YamlPipelineDocument } from '../../lib/flow-editor/helpers/pipelineFlow.types';
import { usePipelineYamlStore } from '../../model/pipelineYamlStore';
import { GraphAdmissionGate } from './GraphAdmissionGate';

const ADMISSIBLE: YamlPipelineDocument = {
  state: { input: 'str', messages: 'list', summary: 'str' },
  entry_point: 'LLM_1',
  nodes: [{ id: 'LLM_1', type: 'llm', input: ['messages'], output: ['summary'], transition: 'END' }],
};

/** The same graph with the `summary` state variable deleted — the LLM node now writes nowhere. */
const INADMISSIBLE: YamlPipelineDocument = { ...ADMISSIBLE, state: { input: 'str', messages: 'list' } };

/** Renders the flag the Save button reads, so the assertions name the real gate. */
function SaveFlagProbe(): ReactNode {
  const form = useForm({ mode: 'onChange', defaultValues: {} });
  return (
    <FormProvider {...form}>
      <span data-testid="can-save">{String(form.formState.isValid)}</span>
      <GraphAdmissionGate />
    </FormProvider>
  );
}

/**
 * Seeds the store the way the real editor does — `yamlCode` and
 * `yamlJsonObject` in step. `yamlCode` used to be a placeholder here
 * (`'seeded'`), which was harmless only while the gate judged
 * `yamlJsonObject`. It judges the live `yamlCode` now, because that is the
 * string `usePipelineGraphDraft` stores.
 */
/**
 * The page's shape: ONE form, and a gate that can come and go under it —
 * `ConfigurationTab.tsx`'s `if (isError) return <error box>` unmounts
 * `GeneralFormPanel`, and therefore this gate, while `EditPipeline.tsx` keeps
 * the Save bar mounted.
 */
function UnmountableGateProbe(): ReactNode {
  const form = useForm({ mode: 'onChange', defaultValues: {} });
  const [mounted, setMounted] = useState(true);
  return (
    <FormProvider {...form}>
      <span data-testid="can-save">{String(form.formState.isValid)}</span>
      <button
        data-testid="toggle-gate"
        onClick={() => setMounted((previous) => !previous)}
      >
        toggle
      </button>
      {mounted && <GraphAdmissionGate />}
    </FormProvider>
  );
}

function seedGraph(document: YamlPipelineDocument): void {
  usePipelineYamlStore.setState({ yamlCode: dumpYaml(document), yamlJsonObject: document });
}

describe('GraphAdmissionGate', () => {
  beforeEach(() => {
    usePipelineYamlStore.setState({ yamlCode: '', yamlJsonObject: {} });
  });

  it('leaves Save available for an admissible graph, and shows no banner', async () => {
    seedGraph(ADMISSIBLE);
    const { getByTestId, queryByTestId } = renderWithTheme(<SaveFlagProbe />);

    await waitFor(() => expect(getByTestId('can-save')).toHaveTextContent('true'));
    expect(queryByTestId('graph-admission-gate')).not.toBeInTheDocument();
  });

  it('blocks Save and names the offending node when the graph is inadmissible', async () => {
    seedGraph(INADMISSIBLE);
    const { getByTestId } = renderWithTheme(<SaveFlagProbe />);

    await waitFor(() => expect(getByTestId('can-save')).toHaveTextContent('false'));
    expect(getByTestId('graph-admission-gate')).toHaveTextContent('LLM_1');
  });

  it('keeps Save available and shows the YAML-only notice for a typed reducer', async () => {
    seedGraph({ ...ADMISSIBLE, state: { ...ADMISSIBLE.state, findings: { type: 'list', value: [], reducer: 'append' } } });
    const { getByTestId, queryByTestId } = renderWithTheme(<SaveFlagProbe />);

    await waitFor(() => expect(getByTestId('can-save')).toHaveTextContent('true'));
    expect(queryByTestId('graph-admission-gate')).not.toBeInTheDocument();
    const notice = getByTestId('graph-admission-notice');
    expect(notice).toHaveTextContent('state.findings');
    expect(notice).toHaveTextContent('YAML');
  });

  it('releases Save again once the graph is fixed', async () => {
    seedGraph(INADMISSIBLE);
    const { getByTestId, queryByTestId } = renderWithTheme(<SaveFlagProbe />);
    await waitFor(() => expect(getByTestId('can-save')).toHaveTextContent('false'));

    /*
     * The discriminator for a real bug in this gate's first shape:
     * react-hook-form's `clearErrors` does not recompute `isValid`
     * (7.83, `dist/index.esm.mjs:2724-2741` never calls `_setValid`), so
     * lifting the veto by clearing the error alone leaves Save disabled
     * forever — the user fixes the graph and can still never save it.
     */
    act(() => seedGraph(ADMISSIBLE));

    await waitFor(() => expect(getByTestId('can-save')).toHaveTextContent('true'));
    expect(queryByTestId('graph-admission-gate')).not.toBeInTheDocument();
  });

  it('does not block Save when the editor holds no graph at all', async () => {
    const { getByTestId } = renderWithTheme(<SaveFlagProbe />);

    // An unseeded editor is not an invalid pipeline. Gating here would block
    // saves that have nothing to do with the graph — see `useGraphAdmission.ts`.
    await waitFor(() => expect(getByTestId('can-save')).toHaveTextContent('true'));
  });

  it('does not throw with no FormProvider ancestor', () => {
    seedGraph(INADMISSIBLE);

    expect(() => renderWithTheme(<GraphAdmissionGate />)).not.toThrow();
  });

  /**
   * THE STUCK-SAVE BUG. The gate can unmount WHILE VETOING and come back:
   * `ConfigurationTab` swaps `GeneralFormPanel` (and therefore this gate) for
   * an error box whenever the detail refetch errors — one now fires after
   * every save — while `EditPipeline` keeps the Save bar mounted.
   *
   * With the veto's record of itself living only in a component-local ref,
   * the remount saw `isVetoing.current === false`, returned early, and never
   * ran the `trigger()` that recomputes `isValid`: Save stayed disabled on a
   * FIXED graph, with no banner left to explain it, and nothing on that page
   * could recover it — the configuration form is still a disclosed gap, so
   * there is no registered input whose edit would re-run the resolver.
   */
  it('releases Save after the gate unmounts while vetoing and remounts onto a fixed graph', async () => {
    seedGraph(INADMISSIBLE);
    /*
     * The FORM stays mounted and only the GATE goes away — that is the real
     * shape, and it is the shape that discriminates. Re-rendering a fresh
     * `useForm` alongside the gate would pass against the bug too, because a
     * brand-new form has no veto published on it.
     */
    const { getByTestId } = renderWithTheme(<UnmountableGateProbe />);
    await waitFor(() => expect(getByTestId('can-save')).toHaveTextContent('false'));

    await userEvent.setup().click(getByTestId('toggle-gate'));
    act(() => seedGraph(ADMISSIBLE));
    await userEvent.setup().click(getByTestId('toggle-gate'));

    await waitFor(() => expect(getByTestId('can-save')).toHaveTextContent('true'));
  });

  /**
   * A document that is not YAML at all reaches the gate only from the Yaml
   * tab, and only because that tab edits the stored string directly. The
   * runtime's refusal for it is `serde_yaml`'s, before any transcribed rule
   * runs — so it carries no issue and needs its own line in the banner.
   */
  it('blocks Save and explains itself when the live yaml does not parse', async () => {
    /*
     * Starts from an ADMISSIBLE graph and waits for Save to be genuinely
     * enabled before breaking the YAML. Asserting `false` from a cold mount
     * would pass against the bug: react-hook-form publishes `isValid: false`
     * for one tick before its first validation pass, so the wait would be
     * satisfied by that window rather than by the veto.
     */
    seedGraph(ADMISSIBLE);
    const { getByTestId } = renderWithTheme(<SaveFlagProbe />);
    await waitFor(() => expect(getByTestId('can-save')).toHaveTextContent('true'));

    act(() => usePipelineYamlStore.setState({ yamlCode: 'nodes: [\n  - id: "unterminated' }));

    await waitFor(() => expect(getByTestId('can-save')).toHaveTextContent('false'));
    expect(getByTestId('graph-admission-gate')).toHaveTextContent('does not parse');
  });

  it('withholds the banner when the configuration panel is collapsed, but still blocks Save', async () => {
    seedGraph(INADMISSIBLE);
    const Collapsed = (): ReactNode => {
      const form = useForm({ mode: 'onChange', defaultValues: {} });
      return (
        <FormProvider {...form}>
          <span data-testid="can-save">{String(form.formState.isValid)}</span>
          <GraphAdmissionGate summaryHidden />
        </FormProvider>
      );
    };
    const { getByTestId, queryByTestId } = renderWithTheme(<Collapsed />);

    await waitFor(() => expect(getByTestId('can-save')).toHaveTextContent('false'));
    expect(queryByTestId('graph-admission-gate')).not.toBeInTheDocument();
  });
});
