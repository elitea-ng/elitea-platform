/**
 * Turns off Zod's JIT before any schema is defined.
 *
 * Zod decides at schema construction whether to compile a fast parser with
 * `new Function`, after a probe that calls it inside a try/catch. The served
 * CSP has no 'unsafe-eval', so the probe is refused and the browser reports a
 * script-src violation on every page load even though Zod falls back. Under
 * `jitless` the probe never runs. Entry points import this module FIRST, so it
 * evaluates before any module that defines a schema.
 */
import { z } from 'zod';

z.config({ jitless: true });
