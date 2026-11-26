/**
 * One composer submit at a time (#3077), decided before the first await: Enter pressed again while a send, a steer or
 * a retry is in flight is ignored, though the input stays open for typing. The input is cleared (`clear`) once the
 * submit resolves `true`, still inside the gate, so a press right after finds it empty.
 */
import { useState } from 'react';

import { createSubmitGate } from '@elitea/client-core/conversation';

/**
 * `submit(run, sent?)`: runs `run` unless a submit is in flight; resolves `true` once it went, after the input was
 * cleared and `sent` (what else the submit clears, such as the attached images) ran.
 */
export function useComposerSubmit(clear: () => void): (run: () => Promise<boolean>, sent?: () => void) => Promise<boolean> {
  const [gate] = useState(createSubmitGate);
  return async (run, onSent) => {
    const sent = await gate.run(async () => {
      const ok = await run();
      if (ok) {
        clear();
        onSent?.();
      }
      return ok;
    });
    return sent === true;
  };
}
