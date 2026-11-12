import { afterEach, describe, expect, it, vi } from 'vitest';

import { openCreateCredentialTab } from './openCreateCredentialTab';

afterEach(() => {
  vi.restoreAllMocks();
});

describe('openCreateCredentialTab', () => {
  it('opens the create-credential page in a new tab and leaves this one (and the toolkit form) alone', () => {
    const open = vi.spyOn(window, 'open').mockReturnValue(null);
    const buildLocation = vi.fn().mockReturnValue({ href: '/credentials/create-credential/github' });

    openCreateCredentialTab({ buildLocation } as never, 'github');

    expect(buildLocation).toHaveBeenCalledWith({
      to: '/credentials/create-credential/$credentialType',
      params: { credentialType: 'github' },
    });
    expect(open).toHaveBeenCalledWith('/credentials/create-credential/github', '_blank', 'noopener');
  });
});
