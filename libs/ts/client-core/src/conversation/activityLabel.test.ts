import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { describe, it } from 'node:test';

import { agentStateOf, activityOf } from './avatarState';
import { ACTIVITY_LABELS_EN, MAX_ACTIVITY_LABEL, activityLabel } from './activityLabel';

const fixture = JSON.parse(
  readFileSync(new URL('../../../../proto/contracts/agent-state-activity-v1.json', import.meta.url), 'utf8'),
) as { desktop: { name: string; state: string; activity?: unknown }[]; cloud: { name: string; state: string; activity?: unknown }[] };

const names = (id: number) => ({ 88: 'CI Doctor', 89: 'Docs Writer' })[id];

describe('activityLabel', () => {
  it('labels every row of the shared events table (ADR-0061 decision 4b)', () => {
    for (const row of [...fixture.desktop, ...fixture.cloud]) {
      const state = agentStateOf(row.state);
      assert.ok(state, row.name);
      const label = activityLabel(state, activityOf(row.activity), names);
      assert.ok(label.length > 0 && label.length <= MAX_ACTIVITY_LABEL, `${row.name}: ${label}`);
      assert.doesNotMatch(label, /secret|--/, row.name);
    }
  });

  it('names the tool, the program, the peer and the hand-off receivers', () => {
    assert.equal(activityLabel('thinking', undefined), 'Thinking');
    assert.equal(activityLabel('speaking', undefined), 'Writing');
    assert.equal(activityLabel('working', { tool: 'run_command', program: 'cargo' }), 'Running cargo');
    assert.equal(activityLabel('working', { tool: 'jira_get_issue' }), 'Running jira_get_issue');
    assert.equal(activityLabel('working', undefined), 'Working');
    assert.equal(activityLabel('waiting', { waiting_on: 'person' }), 'Waiting for you');
    assert.equal(activityLabel('waiting', undefined), 'Waiting for you');
    assert.equal(activityLabel('waiting', { waiting_on: 'agent', peer_participant_id: 88 }, names), 'Waiting for CI Doctor');
    assert.equal(activityLabel('waiting', { waiting_on: 'agent' }, names), 'Waiting for another agent');
    assert.equal(activityLabel('success', { handoff_to: [88, 89] }, names), 'Handed to CI Doctor, Docs Writer');
    assert.equal(activityLabel('success', undefined), 'Done');
    assert.equal(activityLabel('error', undefined), 'Error');
    assert.equal(activityLabel('idle', undefined), 'Idle');
  });

  it('never returns more than 80 characters', () => {
    const long = () => 'N'.repeat(200);
    for (const label of [
      activityLabel('waiting', { waiting_on: 'agent', peer_participant_id: 1 }, long),
      activityLabel('success', { handoff_to: [1, 2, 3, 4] }, long),
      activityLabel('working', { tool: 'x'.repeat(64) }, long, { ...ACTIVITY_LABELS_EN, running: `${'Long '.repeat(10)}{tool}` }),
    ]) {
      assert.ok(label.length <= MAX_ACTIVITY_LABEL, label);
    }
  });

  it('takes the client translation', () => {
    assert.equal(activityLabel('working', { tool: 'grep' }, names, { ...ACTIVITY_LABELS_EN, running: 'Führt {tool} aus' }), 'Führt grep aus');
  });
});
