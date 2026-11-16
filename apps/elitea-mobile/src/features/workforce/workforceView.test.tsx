import { render, screen } from '@testing-library/react-native';

import { MarketplaceProposalsView, WORKFORCE_HINT } from '@/features/workforce/MarketplaceProposalsView';
import { probationText } from '@/features/workforce/ProbationBadge';

jest.mock('react-native-safe-area-context', () => require('react-native-safe-area-context/jest/mock').default);
jest.mock('@/theme/brandStore', () => ({ useBrandTheme: () => null }));
jest.mock('@/theme/brandFonts', () => ({ useBrandFontFamily: () => undefined }));

const rows = [
  { id: 9, kind: 'hire_listing' as const, listingName: 'PR Reader', versionName: 'v2.3', mode: 'install' as const, outcome: '', stage: '', text: 'It reads <b>pull</b> requests.' },
  { id: 10, kind: 'feedback_report' as const, listingName: 'PR Reader', versionName: '', mode: null, outcome: 'dismissed', stage: 'retire', text: 'It failed often.' },
];

describe('MarketplaceProposalsView', () => {
  it('lists the proposals as plain text and says where to hire', async () => {
    await render(<MarketplaceProposalsView status={{ kind: 'ready' }} rows={rows} selectable={false} onRetry={() => {}} />);
    expect(screen.getByText('Hire PR Reader v2.3 (install)')).toBeTruthy();
    expect(screen.getByText('Report on PR Reader: dismissed at retire')).toBeTruthy();
    // Agent-written text is shown as it was written, never interpreted.
    expect(screen.getByText('It reads <b>pull</b> requests.')).toBeTruthy();
    expect(screen.getByText(WORKFORCE_HINT)).toBeTruthy();
  });

  it('says so for a caller the server refused', async () => {
    await render(
      <MarketplaceProposalsView status={{ kind: 'error', message: 'Only an agent manager sees the workforce proposals of this project.' }} rows={[]} selectable={false} onRetry={() => {}} />,
    );
    expect(screen.getByText(/Only an agent manager/)).toBeTruthy();
  });

  it('says so for an empty list', async () => {
    await render(<MarketplaceProposalsView status={{ kind: 'ready' }} rows={[]} selectable={false} onRetry={() => {}} />);
    expect(screen.getByText('No proposals')).toBeTruthy();
  });
});

describe('probationText', () => {
  it('names the end of the probation, or just says so', () => {
    expect(probationText(null)).toBe('On probation');
    expect(probationText('2026-11-24T00:00:00Z')).toMatch(/^On probation until /);
  });
});
