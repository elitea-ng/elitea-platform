/**
 * The slice's public API: ONE component.
 *
 * `EvaluationPanel` is the whole of what the agent editor mounts, and every
 * other symbol in this slice is reached through it. An earlier draft of this
 * barrel re-exported the library view, the editor dialog, five form helpers,
 * the dimension hooks and every run hook — 25 symbols for one consumer.
 *
 * That is not free. `knip.json` marks every feature barrel as an ENTRY POINT,
 * so anything re-exported here becomes reachable and the dead-code gate stops
 * being able to see it. A barrel that lists everything turns the one gate that
 * finds unused code into a gate that guarantees it never will. The 20-symbol
 * budget in check-budgets is the mechanism that keeps that from happening
 * quietly.
 */
export { EvaluationPanel } from './ui/EvaluationPanel';
