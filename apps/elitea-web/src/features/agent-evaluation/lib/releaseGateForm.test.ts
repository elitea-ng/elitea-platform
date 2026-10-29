import { describe, expect, it } from 'vitest';

import { draftFromSuite, draftProblem, requestFromDraft, type ReleaseGateDraft } from './releaseGateForm';

const draft = (overrides: Partial<ReleaseGateDraft> = {}): ReleaseGateDraft => ({
  releaseGate: true,
  regression: false,
  criteria: [{ dimensionId: '5', kind: 'threshold', value: '70' }],
  reevaluateEveryDays: '',
  ...overrides,
});

describe('release gate form', () => {
  it('turns a threshold and a no_worse_than criterion into the server shape', () => {
    expect(
      requestFromDraft(
        draft({
          regression: true,
          criteria: [
            { dimensionId: '5', kind: 'threshold', value: '70' },
            { dimensionId: '6', kind: 'no_worse_than', value: '2.5' },
          ],
          reevaluateEveryDays: '30',
        }),
      ),
    ).toEqual({
      release_gate: true,
      regression: true,
      criteria: [
        { dimension_id: '5', kind: 'threshold', min_score: 70 },
        { dimension_id: '6', kind: 'no_worse_than', max_drop: 2.5 },
      ],
      reevaluate_every_days: 30,
    });
  });

  it('unmarks with release_gate false and nothing else', () => {
    expect(requestFromDraft(draft({ releaseGate: false }))).toEqual({ release_gate: false });
  });

  it('refuses what the server refuses', () => {
    expect(draftProblem(draft({ criteria: [] }))).toBe('criteria');
    expect(
      draftProblem(
        draft({
          criteria: [
            { dimensionId: '5', kind: 'threshold', value: '1' },
            { dimensionId: '5', kind: 'no_worse_than', value: '1' },
          ],
        }),
      ),
    ).toBe('duplicate');
    expect(draftProblem(draft({ criteria: [{ dimensionId: '5', kind: 'threshold', value: '101' }] }))).toBe('value');
    expect(draftProblem(draft({ reevaluateEveryDays: '0' }))).toBe('cadence');
    expect(draftProblem(draft())).toBeUndefined();
  });

  it('reads a stored suite back into the form', () => {
    expect(
      draftFromSuite({
        application_id: 7,
        dataset_id: 3,
        release_gate: true,
        regression: true,
        criteria: [{ dimension_id: '6', kind: 'no_worse_than', max_drop: 3 }],
        reevaluate_every_days: null,
      }),
    ).toEqual({
      releaseGate: true,
      regression: true,
      criteria: [{ dimensionId: '6', kind: 'no_worse_than', value: '3' }],
      reevaluateEveryDays: '',
    });
  });
});
