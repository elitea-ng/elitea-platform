import React, { memo } from 'react';

/**
 * Indeterminate upload spinner — issue #877. There is no byte-level
 * progress signal for this widget's attachment upload (it goes through
 * `eliteaFetch`, not the XHR-based `shared/api/upload.ts` the main chat
 * composer uses for its progress events), so this spins at a constant rate
 * rather than tracking a fake percentage — `input.css`'s
 * `elitea-assistant-file-chip-spinner` class supplies the rotation.
 */
const SpinnerIcon: React.FC = memo(() => (
  <svg
    className="elitea-assistant-file-chip-spinner-spin"
    viewBox="0 0 24 24"
    xmlns="http://www.w3.org/2000/svg"
    fill="none"
  >
    <circle cx="12" cy="12" r="9" stroke="currentColor" strokeWidth="3" opacity="0.25" />
    <path d="M21 12a9 9 0 0 0-9-9" stroke="currentColor" strokeWidth="3" strokeLinecap="round" />
  </svg>
));

SpinnerIcon.displayName = 'SpinnerIcon';

export default SpinnerIcon;
