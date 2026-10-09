/**
 * An in-memory `DoctorIpc` for tests and the dev harness: `checks` is what
 * `run` answers; `fix` applies `repairs[fixId]` (a function returning the
 * checks after the repair) and records the call.
 */
import type { DoctorCheck, DoctorIpc } from './doctorIpc';
import { WorkspaceIpcError } from './workspaceIpc';

export interface FakeDoctorIpc extends DoctorIpc {
  /** `fixes`: the repairs applied; `refused`: those asked without the confirmation their check requires. */
  readonly calls: { runs: ('local' | 'all')[]; fixes: string[]; refused: string[] };
}

export function createFakeDoctorIpc(
  initial: DoctorCheck[],
  repairs: Record<string, (checks: DoctorCheck[]) => DoctorCheck[]> = {},
): FakeDoctorIpc {
  let checks = initial;
  const calls: FakeDoctorIpc['calls'] = { runs: [], fixes: [], refused: [] };
  return {
    calls,
    run(scope) {
      calls.runs.push(scope ?? 'all');
      const shown = scope === 'local' ? checks.filter((c) => !['deployment', 'session', 'local_work', 'pending_revokes'].includes(c.id)) : checks;
      return Promise.resolve(shown.map((c) => ({ ...c })));
    },
    fix(fixId, confirm) {
      // As the host: a repair that deletes data runs only once confirmed.
      if (confirm !== true && checks.some((c) => c.fix_id === fixId && c.fix_confirm !== undefined)) {
        calls.refused.push(fixId);
        return Promise.reject(new WorkspaceIpcError('unknown', 'this repair deletes what Elitea keeps for these folders; confirm it first'));
      }
      calls.fixes.push(fixId);
      const repair = repairs[fixId];
      if (repair === undefined) return Promise.reject(new WorkspaceIpcError('storage', `unknown repair \`${fixId}\``));
      checks = repair(checks);
      return Promise.resolve('Fixed.');
    },
  };
}
