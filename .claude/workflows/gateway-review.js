export const meta = {
  name: 'gateway-review',
  description: 'Self-serve rotating-lens review of the elitea-llm-gateway change set: structural + subtle + audit-the-fixes lenses, adversarial verify, mergeability verdict. Run on any branch/PR without a human driving it.',
  whenToUse: 'Before merging any PR that touches the LLM gateway / budget enforcement / edge. Distilled from 3 manual review rounds that found ~91 findings (12+ high) on a green-CI PR.',
  phases: [
    { title: 'Review', detail: 'lenses across the diff (structural, subtle, audit-the-fixes, security, money, spec)' },
    { title: 'Verify', detail: 'adversarially confirm each finding is real + reachable' },
    { title: 'Synthesize', detail: 'dedupe, rank, tag autoFixable, mergeability verdict' },
  ],
}

// args = diff base ref (default: origin/main). Pass a ref to review a branch.
const BASE = (typeof args === 'string' && args.trim()) ? args.trim() : 'origin/main'
const ROOT = 'services/elitea-llm-gateway'
const BUILD = `Gateway is a standalone go1.27 module (go.mod 1.27.3; the workspace is on 1.26) — build/test with \`cd ${ROOT} && GOWORK=off go test -race ./...\` and \`GOWORK=off golangci-lint run ./...\`. It is NOT in go.work. elitea-main/scheduler are in go.work. Money is int64 nano-USD (never float). Authoritative policy: ${ROOT}/CLAUDE.md + DECISIONS.md.`
const CONTEXT = `Review the CHANGED code on this branch (git diff against ${BASE}). This is the LiteLLM->Bifrost gateway (budget enforcement + edge). Prior review found bugs in three recurring classes — hunt them specifically: (1) BUILT-BUT-NOT-WIRED: a component/lifecycle method exists + unit-tests green but is never called from the composition root (cmd/.../main.go) — verify every Start/Drain/gate is actually invoked; (2) HELM ENV DRIFT: code reads an env var the chart can't set; (3) UNMETERED PATH: a /llm endpoint that skips the budget gate. Also: money-path precision, fail-closed policy, trust boundary, spec §2.5 error shapes, concurrency, and TEST QUALITY (tests that pass without proving their claim). Be adversarial: file:line + a concrete failing scenario. Empty findings for a sound area is a valid, useful answer.`

const F_ITEM = {
  type: 'object', additionalProperties: false,
  required: ['file', 'line', 'severity', 'category', 'summary', 'scenario', 'fix'],
  properties: {
    file: { type: 'string' }, line: { type: 'integer' },
    severity: { type: 'string', description: 'high|medium|low' },
    category: { type: 'string' },
    summary: { type: 'string' }, scenario: { type: 'string' }, fix: { type: 'string' },
  },
}
const F_SCHEMA = { type: 'object', additionalProperties: false, required: ['findings'], properties: { findings: { type: 'array', items: F_ITEM } } }

const LENSES = [
  { key: 'wiring', prompt: 'BUILT-BUT-NOT-WIRED: for every constructor/lifecycle method in the diff (NewX, Start, Drain, DrainBilling, Close, WithBudgetGate), is it actually called from cmd/.../main.go and guarded by TestMainWiring? Flag anything present-but-not-wired.' },
  { key: 'env-drift', prompt: 'HELM ENV DRIFT: does any env var read by config.go/vault.go in the diff lack a chart setter (values.yaml / deployment.yaml) and an allowlist entry? Run scripts/env-drift-check.sh and reason about new vars.' },
  { key: 'unmetered', prompt: 'UNMETERED PATH: does every /llm endpoint (incl. any added/changed) call checkBudget BEFORE the provider and updateUsage after? Any handler that dispatches to core without the gate.' },
  { key: 'money', prompt: 'MONEY PATH: any float on the nano-USD path, overflow, rounding drift, double-count, per-1M vs per-1k confusion, denomination mismatch across the write-behind hop, in the diff.' },
  { key: 'security', prompt: 'SECURITY: trust-boundary (X-Auth-*/X-Elitea-* only from trusted proxy; client auth stripped), HMAC, self-referential-credential bypass, credential/token logging, fail-open holes.' },
  { key: 'spec-concurrency', prompt: 'SPEC §2.5 + CONCURRENCY: error bodies OpenAI-shaped on all /llm with correct type/code per status; and data races / goroutine leaks / WaitGroup misuse / context handling in the changed code (reason about -race).' },
  { key: 'test-quality', prompt: 'TEST QUALITY: do the tests in the diff PROVE their claim, or pass vacuously (assert on a fake, sleep-based guard, tautology, missing negative case)? Name specific weak tests + what they fail to catch.' },
]

phase('Review')
const reviewed = await parallel(LENSES.map(l => () =>
  agent(`${CONTEXT}\n\n${BUILD}\n\nLENS: ${l.key}\n${l.prompt}\n\nReport concrete findings only: file:line + failing scenario. Empty is valid.`,
    { label: `rev:${l.key}`, phase: 'Review', effort: 'high', schema: F_SCHEMA })
    .then(x => ({ key: l.key, findings: (x && x.findings) || [] }))
))
const cand = reviewed.filter(Boolean).flatMap(r => r.findings.map(f => ({ ...f, lens: r.key })))
log(`Review: ${cand.length} candidate findings`)

phase('Verify')
const V_SCHEMA = { type: 'object', additionalProperties: false, required: ['confirmed', 'reason'], properties: { confirmed: { type: 'boolean' }, reason: { type: 'string' }, severityAdjust: { type: 'string' } } }
const verified = await parallel(cand.map(f => () =>
  agent(`${BUILD}\n\nAdversarially VERIFY this ${f.lens} finding against the real code. Open ${f.file} near line ${f.line}.\nCLAIM: ${f.summary}\nSCENARIO: ${f.scenario}\n\nConfirm ONLY if real AND reachable (construct the failing input) or find the code/test that already prevents it. Default confirmed=false if unsure.`,
    { label: `ver:${f.lens}:${f.line}`, phase: 'Verify', schema: V_SCHEMA })
    .then(v => ({ ...f, verdict: v }))))
const confirmed = verified.filter(Boolean).filter(f => f.verdict && f.verdict.confirmed)
  .map(f => ({ ...f, severity: f.verdict.severityAdjust || f.severity }))
log(`Verify: ${confirmed.length} confirmed of ${cand.length}`)

phase('Synthesize')
const sev = { high: 0, medium: 1, low: 2 }
const ranked = confirmed.sort((a, b) => (sev[a.severity] ?? 3) - (sev[b.severity] ?? 3))
const D_ITEM = {
  type: 'object', additionalProperties: false,
  required: ['severity', 'category', 'file', 'summary', 'fix', 'autoFixable'],
  properties: { severity: { type: 'string' }, category: { type: 'string' }, file: { type: 'string' }, line: { type: 'integer' }, summary: { type: 'string' }, fix: { type: 'string' }, autoFixable: { type: 'boolean' } },
}
const S_SCHEMA = {
  type: 'object', additionalProperties: false, required: ['verdict', 'mergeable', 'deduped'],
  properties: {
    verdict: { type: 'string' },
    mergeable: { type: 'string', description: 'MERGE_OK | FIX_FIRST | NEEDS_HUMAN' },
    deduped: { type: 'array', items: D_ITEM },
  },
}
const synth = await agent(
  `${CONTEXT}\n\n${ranked.length} adversarially-verified findings:\n\n` +
  ranked.map((f, i) => `${i}. [${f.severity}/${f.category}/${f.lens}] ${f.file}:${f.line} — ${f.summary}\n   fix: ${f.fix}`).join('\n') +
  `\n\nDedupe (same root cause = one), drop acceptable trade-offs, produce the final prioritized list with autoFixable tags (autoFixable=false for anything touching fail-open/closed policy, the trust boundary, money denomination, or the async-billing bound — those are human decisions per DECISIONS.md). Then give an honest verdict and a mergeability call: MERGE_OK (only nits) | FIX_FIRST (fixable blockers) | NEEDS_HUMAN (policy/design decisions remain).`,
  { label: 'synth', phase: 'Synthesize', effort: 'high', schema: S_SCHEMA })

return { base: BASE, counts: { candidates: cand.length, confirmed: confirmed.length, final: (synth.deduped || []).length }, verdict: synth.verdict, mergeable: synth.mergeable, findings: synth.deduped || [] }
