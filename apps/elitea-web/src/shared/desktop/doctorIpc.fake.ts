/**
 * An in-memory `DoctorIpc` for tests and the dev harness: `checks` is what
 * `run` answers; `fix` applies `repairs[fixId]` (a function returning the
 * checks after the repair) and records the call.
 */
import type { DoctorCheck, DoctorIpc } from './doctorIpc';
import { WorkspaceIpcError } from './workspaceIpc';

export interface FakeDoctorIpc extends DoctorIpc {
  readonly calls: { runs: ('local' | 'all')[]; fixes: string[] };
}

export function createFakeDoctorIpc(
  initial: DoctorCheck[],
  repairs: Record<string, (checks: DoctorCheck[]) => DoctorCheck[]> = {},
): FakeDoctorIpc {
  let checks = initial;
  const calls: FakeDoctorIpc['calls'] = { runs: [], fixes: [] };
  return {
    calls,
    run(scope) {
      calls.runs.push(scope ?? 'all');
      const shown = scope === 'local' ? checks.filter((c) => !['deployment', 'session', 'local_work', 'pending_revokes'].includes(c.id)) : checks;
      return Promise.resolve(shown.map((c) => ({ ...c })));
    },
    fix(fixId) {
      calls.fixes.push(fixId);
      const repair = repairs[fixId];
      if (repair === undefined) return Promise.reject(new WorkspaceIpcError('storage', `unknown repair \`${fixId}\``));
      checks = repair(checks);
      return Promise.resolve('Fixed.');
    },
  };
}
