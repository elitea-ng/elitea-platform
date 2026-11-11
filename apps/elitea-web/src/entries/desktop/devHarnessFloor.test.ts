import { describe, expect, it } from 'vitest';

import { floorAnswer, floorHarness } from './devHarnessFloor';

const thread = '/api/v2/elitea_core/conversation/prompt_lib/42/77';
const added = JSON.stringify([{ entity_name: 'application', entity_meta: { id: 6 } }]);

describe('floorHarness', () => {
  it('starts each harness with no lead and no adds carried over from an earlier one (#1932)', () => {
    const first = floorHarness(() => undefined, () => 't1');
    first.setLead(88);
    floorAnswer('POST', '/api/v2/elitea_core/participants/prompt_lib/42/77', added);
    expect(first.participantPosts).toHaveLength(1);
    expect(floorAnswer('GET', thread, undefined)).toBeDefined();

    const second = floorHarness(() => undefined, () => 't2');
    expect(second.participantPosts).toHaveLength(0);
    expect(floorAnswer('GET', thread, undefined)).toBeUndefined();
  });
});
