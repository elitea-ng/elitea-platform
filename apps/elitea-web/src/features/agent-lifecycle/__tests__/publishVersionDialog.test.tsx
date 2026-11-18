/**
 * The Publish wizard's FAIL guard (#2032): pressing Publish on a failed
 * validation must say why on screen, not do nothing.
 */
import { screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import type { NormalizedPublishValidation } from '../lib/publishValidation';
import { PublishVersionDialog } from '../ui/PublishVersionDialog';
import { renderWithProviders } from './testUtils';

const FAILED: NormalizedPublishValidation = {
  status: 'FAIL',
  criticalIssues: [{ id: 'c1', text: 'The model has no provider.' }],
  warnings: [],
  recommendations: [],
};

describe('PublishVersionDialog on a failed validation', () => {
  it('shows the failure reason when Publish is pressed, and publishes nothing', async () => {
    const onPublish = vi.fn();
    renderWithProviders(
      <PublishVersionDialog
        open
        validation={FAILED}
        isValidating={false}
        isPublishing={false}
        error={undefined}
        onClose={vi.fn()}
        onValidate={vi.fn()}
        onPublish={onPublish}
      />,
    );
    expect(screen.queryByTestId('publish-blocked-reason')).toBeNull();
    await userEvent.click(screen.getByRole('button', { name: 'Publish' }));
    expect(onPublish).not.toHaveBeenCalled();
    expect(screen.getByTestId('publish-blocked-reason')).toHaveTextContent('The model has no provider.');
  });
});
