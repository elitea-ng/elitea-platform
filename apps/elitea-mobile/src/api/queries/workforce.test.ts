import { marketplaceProposalsOf, probationOf, proposalsErrorText, proposalTitle } from '@/api/queries/workforce';
import { ApiError } from '@/api/errors';

const hire = {
  id: 9,
  kind: 'hire_listing',
  status: 'proposed',
  stage: '',
  rationale: 'It reads pull requests.',
  payload: { listing_name: 'PR Reader', version_name: 'v2.3', mode: 'install' },
};
const report = {
  id: 10,
  kind: 'feedback_report',
  status: 'asked_owner',
  stage: 'retire',
  rationale: 'It failed often.',
  payload: { listing_name: 'PR Reader', outcome: 'dismissed' },
};

describe('workforce proposals', () => {
  it('keeps open hires and reports, drops decided ones, ladder kinds and malformed rows', () => {
    const rows = marketplaceProposalsOf({
      rows: [
        hire,
        report,
        { ...hire, id: 11, status: 'approved' },
        { ...hire, id: 12, kind: 'dismiss' },
        { id: 'x' },
        'nope',
      ],
    });
    expect(rows.map((row) => row.id)).toEqual([9, 10]);
    expect(rows[0]).toMatchObject({ kind: 'hire_listing', mode: 'install', versionName: 'v2.3' });
    expect(rows[1]).toMatchObject({ kind: 'feedback_report', outcome: 'dismissed', stage: 'retire' });
  });

  it('writes the title as text', () => {
    const [first, second] = marketplaceProposalsOf({ rows: [hire, report] });
    expect(proposalTitle(first!)).toBe('Hire PR Reader v2.3 (install)');
    expect(proposalTitle(second!)).toBe('Report on PR Reader: dismissed at retire');
  });

  it('answers nothing for a body that is not a list', () => {
    expect(marketplaceProposalsOf(null)).toEqual([]);
    expect(marketplaceProposalsOf({ rows: 'x' })).toEqual([]);
  });

  it('says only an agent manager reads them when the server answers 403', () => {
    expect(proposalsErrorText(new ApiError('http', 'forbidden', { status: 403 }))).toMatch(/Only an agent manager/);
    expect(proposalsErrorText(new ApiError('network', 'offline'))).toBe('You appear to be offline.');
    expect(proposalsErrorText(new Error('x'))).toBe('The proposals could not be loaded.');
  });
});

describe('probation', () => {
  it('is on probation until a manager keeps the hire', () => {
    expect(probationOf({ auto_hired_at: '2026-11-10T00:00:00Z', probation_until: '2026-11-24T00:00:00Z', probation_reviewed_at: null, lifecycle_state: 'active' })).toEqual({
      until: '2026-11-24T00:00:00Z',
    });
    expect(probationOf({ auto_hired_at: '2026-11-10T00:00:00Z', probation_reviewed_at: '2026-11-12T00:00:00Z', lifecycle_state: 'active' })).toBeNull();
    expect(probationOf({ auto_hired_at: null, lifecycle_state: 'active' })).toBeNull();
    expect(probationOf({ auto_hired_at: '2026-11-10T00:00:00Z', probation_reviewed_at: null, lifecycle_state: 'retired' })).toBeNull();
    expect(probationOf('nope')).toBeNull();
  });
});
