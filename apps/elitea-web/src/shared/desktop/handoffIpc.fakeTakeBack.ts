/**
 * The take-back part of the fake host's answers (`handoffIpc.fake.ts`): the
 * preflight and apply it gives until a test scripts its own.
 */
import type { ApplyResult, Preflight } from './handoffIpc';

/** An apply answer; a test may leave the report out. */
export type FakeApply = Omit<ApplyResult, 'report'> & { report?: ApplyResult['report'] };

export const DEFAULT_PREFLIGHT: Preflight = {
  preflight_id: 'pf-1',
  outcome: 'fast_path',
  applied: false,
  changed_paths: [{ path: 'src/main.rs', change: 'modified', from: null }],
  conflict_paths: [],
  skipped_paths: [],
  base: 'b'.repeat(40),
  result_tree: 'c'.repeat(40),
  cloud_turns: 1,
  up_to_turn: 1,
  modes: ['apply', 'worktree', 'discard'],
  files: [{ path: 'src/main.rs', status: 'modified', added: 1, removed: 1, diff: '--- a/src/main.rs\n+++ b/src/main.rs\n@@ -1 +1 @@\n-fn main() {}\n+fn main() { run() }\n' }],
  diffs_truncated: false,
};

export const DEFAULT_APPLY: FakeApply = {
  applied: true,
  outcome: 'applied',
  checkpoint_id: 'w1:7',
  worktree_path: null,
  branch: 'elitea/cloud/abcd1234',
  imported_turns: 1,
  turns_not_taken: 0,
  rest_branch: null,
  released: true,
  copied: [],
};
