import { screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';

import type { HandoffStatus } from '@/shared/desktop/handoffIpc';
import { LOCAL_STATUS } from '@/shared/desktop/handoffIpc.fake';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { ThreadCloudBadge } from './ThreadCloudBadge';

const mount = (status: HandoffStatus | undefined): void => {
  renderWithTheme(<ThreadCloudBadge status={status} />);
};

describe('ThreadCloudBadge', () => {
  it('shows In cloud for a thread running in the cloud', () => {
    mount({ ...LOCAL_STATUS, state: 'CLOUD_IDLE', executor: 'cloud' });
    expect(screen.getByTestId('executor-badge')).toHaveTextContent('In cloud');
  });

  it('shows Returning for a thread on its way back', () => {
    mount({ ...LOCAL_STATUS, state: 'RECLAIMING', executor: 'returning' });
    expect(screen.getByTestId('executor-badge')).toHaveTextContent('Returning');
  });

  it('shows nothing for a thread that runs here, or whose status is not known yet', () => {
    mount(LOCAL_STATUS);
    expect(screen.queryByTestId('executor-badge')).toBeNull();
    mount(undefined);
    expect(screen.queryByTestId('executor-badge')).toBeNull();
  });
});
