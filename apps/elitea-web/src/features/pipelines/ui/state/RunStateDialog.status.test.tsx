import { fireEvent, screen } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';

import { DEFAULT_BRAND_PACK, buildEliteaTheme } from '@/shared/brand';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { FlowEditorConstants } from '../../lib/flow-editor/constants';

import { HeaderActions, StatusIndicatorRow } from './RunStateDialog.status';

const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

/**
 * `theme.vars.palette.*` resolves to a bare `var(--el-...)` reference, but
 * the CSS jsdom actually parses out of the emotion-injected stylesheet
 * serializes that same custom property back out *with* its baked-in
 * fallback (`var(--el-..., #hex)`) — so a direct `toHaveStyle({ color:
 * theme.vars.palette.* })` string-equality check never matches even when
 * the colour is correctly wired. Asserting the custom-property *name* is
 * present in the computed `color` is the same check `RunStateDialog.
 * styles.test.ts` makes structurally (`resolve(...)` object comparison),
 * ported to a rendered-DOM assertion.
 */
function expectColorVar(element: Element | null | undefined, varRef: string): void {
  const varName = /var\((--[\w-]+)/.exec(varRef)?.[1];
  if (!varName) throw new Error(`not a var() reference: ${varRef}`);
  expect(element).not.toBeNull();
  expect(getComputedStyle(element as Element).color).toContain(varName);
}

function renderStatus(status: string) {
  return renderWithTheme(
    <StatusIndicatorRow
      visible
      lastStep={{ id: 'llm_call', status }}
    />,
  );
}

describe('RunStateDialog.status', () => {
  describe('StatusIndicatorRow', () => {
    it('renders nothing when not visible', () => {
      const { container } = renderWithTheme(
        <StatusIndicatorRow
          visible={false}
          lastStep={{ id: 'llm_call', status: FlowEditorConstants.PipelineStatus.Completed }}
        />,
      );
      expect(container).toBeEmptyDOMElement();
    });

    it('InProgress: shows the "{stepId}:" label and "Performing"', () => {
      renderStatus(FlowEditorConstants.PipelineStatus.InProgress);
      expect(screen.getByText('llm_call:')).toBeInTheDocument();
      expect(screen.getByText('Performing')).toBeInTheDocument();
    });

    // Baseline renders `{stepId}:` once, outside/before all 5 status
    // branches — every branch below asserts it independently so a port
    // that only wires the label into some of them (as this file's own
    // history did for `Completed`/`Stopped`) fails here.
    it('Error: shows the "{stepId}:" label and "Error" text in the rejected colour', () => {
      renderStatus(FlowEditorConstants.PipelineStatus.Error);
      expect(screen.getByText('llm_call:')).toBeInTheDocument();
      expectColorVar(screen.getByText('Error'), theme.vars.palette.status.rejected);
    });

    it('Interrupt: shows the "{stepId}:" label, an onModeration-tinted icon and inactive-grey text', () => {
      const { container } = renderStatus(FlowEditorConstants.PipelineStatus.Interrupt);
      expect(screen.getByText('llm_call:')).toBeInTheDocument();
      expectColorVar(screen.getByText('User action waiting...'), theme.vars.palette.icon.fill.inactive);
      const icon = container.querySelector('svg');
      expectColorVar(icon?.parentElement, theme.vars.palette.status.onModeration);
    });

    it('Completed: shows the "{stepId}:" label alongside the "Completed" text', () => {
      renderStatus(FlowEditorConstants.PipelineStatus.Completed);
      expect(screen.getByText('llm_call:')).toBeInTheDocument();
      expectColorVar(screen.getByText('Completed'), theme.vars.palette.status.published);
    });

    it('Stopped: shows the "{stepId}:" label, an onModeration-tinted icon and inactive-grey text', () => {
      const { container } = renderStatus(FlowEditorConstants.PipelineStatus.Stopped);
      expect(screen.getByText('llm_call:')).toBeInTheDocument();
      expectColorVar(screen.getByText('Stopped'), theme.vars.palette.icon.fill.inactive);
      const icon = container.querySelector('svg');
      expectColorVar(icon?.parentElement, theme.vars.palette.status.onModeration);
    });
  });

  describe('HeaderActions', () => {
    it('keeps Stop for a recovery interrupt without offering Delete', () => {
      const onStop = vi.fn();
      renderWithTheme(
        <HeaderActions status={FlowEditorConstants.PipelineStatus.Interrupt} recoveryPaused
          onStop={onStop} onDelete={vi.fn()} onClose={vi.fn()} />,
      );
      expect(screen.queryByRole('button', { name: 'Delete run' })).not.toBeInTheDocument();
      fireEvent.click(screen.getByRole('button', { name: 'Stop run' }));
      expect(onStop).toHaveBeenCalledTimes(1);
    });

    it.each([FlowEditorConstants.PipelineStatus.Completed, FlowEditorConstants.PipelineStatus.Stopped])(
      'ignores a retained recovery flag after %s', (status) => {
        renderWithTheme(
          <HeaderActions status={status} recoveryPaused
            onStop={vi.fn()} onDelete={vi.fn()} onClose={vi.fn()} />,
        );
        expect(screen.queryByRole('button', { name: 'Stop run' })).not.toBeInTheDocument();
        expect(screen.getByRole('button', { name: 'Delete run' })).toBeInTheDocument();
      },
    );

    it('shows Stop (not Delete) while In progress, and always shows Close', () => {
      const onStop = vi.fn();
      const onDelete = vi.fn();
      const onClose = vi.fn();
      renderWithTheme(
        <HeaderActions
          status={FlowEditorConstants.PipelineStatus.InProgress}
          onStop={onStop}
          onDelete={onDelete}
          onClose={onClose}
        />,
      );

      expect(screen.getByRole('button', { name: 'Stop run' })).toBeInTheDocument();
      expect(screen.queryByRole('button', { name: 'Delete run' })).not.toBeInTheDocument();

      fireEvent.click(screen.getByRole('button', { name: 'Stop run' }));
      expect(onStop).toHaveBeenCalledTimes(1);

      fireEvent.click(screen.getByRole('button', { name: 'Close' }));
      expect(onClose).toHaveBeenCalledTimes(1);
    });

    it('shows Delete (not Stop) once the run is no longer in progress', () => {
      const onStop = vi.fn();
      const onDelete = vi.fn();
      const onClose = vi.fn();
      renderWithTheme(
        <HeaderActions
          status={FlowEditorConstants.PipelineStatus.Completed}
          onStop={onStop}
          onDelete={onDelete}
          onClose={onClose}
        />,
      );

      expect(screen.getByRole('button', { name: 'Delete run' })).toBeInTheDocument();
      expect(screen.queryByRole('button', { name: 'Stop run' })).not.toBeInTheDocument();

      fireEvent.click(screen.getByRole('button', { name: 'Delete run' }));
      expect(onDelete).toHaveBeenCalledTimes(1);
    });
  });
});
