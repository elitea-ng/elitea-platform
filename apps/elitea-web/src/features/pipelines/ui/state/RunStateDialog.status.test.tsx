import { fireEvent, screen } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { FlowEditorConstants } from '../../lib/flow-editor/constants';

import { HeaderActions } from './RunStateDialog.status';

describe('RunStateDialog.status', () => {
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
