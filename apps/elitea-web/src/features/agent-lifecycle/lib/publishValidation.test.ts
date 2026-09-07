import { describe, expect, it } from 'vitest';

import { isValidPublishVersionName, normalizePublishValidation, PUBLISH_CATEGORIES } from './publishValidation';

describe('normalizePublishValidation', () => {
  it('carries the declared status and the token through', () => {
    const result = normalizePublishValidation({
      status: 'PASS',
      validation_token: '0123456789abcdef',
      summary: 'All good',
    });
    expect(result.status).toBe('PASS');
    expect(result.validationToken).toBe('0123456789abcdef');
    expect(result.summary).toBe('All good');
  });

  // The server sends `validation_token: null` on FAIL. Passing null through as
  // a token would let the publish call claim validation had already run.
  it('drops the token on a FAIL', () => {
    const result = normalizePublishValidation({
      status: 'FAIL',
      validation_token: null,
      critical_issues: [{ rule: 'generic_name', issue: 'The name is too generic' }],
    });
    expect(result.status).toBe('FAIL');
    expect(result.validationToken).toBeUndefined();
    expect(result.criticalIssues.map((finding) => finding.text)).toEqual(['The name is too generic']);
  });

  // The publish 422 short-circuit carries `issues` and NO status at all. Read
  // as a PASS, the wizard would walk straight past the refusal.
  it('reads a bare `issues` 422 as a FAIL', () => {
    const result = normalizePublishValidation({
      issues: [{ rule: 'version_name_exists_in_source', message: 'version name already exists' }],
    });
    expect(result.status).toBe('FAIL');
    expect(result.criticalIssues).toHaveLength(1);
    expect(result.criticalIssues[0]?.text).toBe('version name already exists');
  });

  it('reads a body with only warnings as a WARN', () => {
    const result = normalizePublishValidation({ warnings: [{ rule: 'no_tags', issue: 'Add at least one tag' }] });
    expect(result.status).toBe('WARN');
    expect(result.warnings[0]?.text).toBe('Add at least one tag');
  });

  it('reads an empty body as a PASS with nothing to report', () => {
    const result = normalizePublishValidation({});
    expect(result.status).toBe('PASS');
    expect(result.criticalIssues).toHaveLength(0);
    expect(result.warnings).toHaveLength(0);
    expect(result.recommendations).toHaveLength(0);
  });

  it('reads an absent body the same way, rather than throwing', () => {
    expect(normalizePublishValidation(undefined).status).toBe('PASS');
  });

  // Recommendations carry `suggestion` and no `issue`/`message`. Reading only
  // `issue` rendered an empty bullet for every one of them.
  it('reads a recommendation out of `suggestion`', () => {
    const result = normalizePublishValidation({
      status: 'WARN',
      recommendations: [{ rule: 'add_starters', suggestion: 'Add conversation starters' }],
    });
    expect(result.recommendations[0]?.text).toBe('Add conversation starters');
  });

  // The cycle/depth criticals carry a `fix`; it is the only remediation text
  // the author gets.
  it('keeps the remediation text of a critical issue', () => {
    const result = normalizePublishValidation({
      status: 'FAIL',
      critical_issues: [{ rule: 'sub_agent_cycle', issue: 'A cycle exists', fix: 'Remove the loop' }],
    });
    expect(result.criticalIssues[0]?.fix).toBe('Remove the loop');
  });

  it('falls back to the rule name rather than rendering a blank finding', () => {
    const result = normalizePublishValidation({ status: 'FAIL', critical_issues: [{ rule: 'unknown_rule' }] });
    expect(result.criticalIssues[0]?.text).toBe('unknown_rule');
  });

  it('gives every finding a distinct key, even when two share a rule', () => {
    const result = normalizePublishValidation({
      status: 'FAIL',
      critical_issues: [{ rule: 'same', issue: 'one' }, { rule: 'same', issue: 'two' }],
    });
    expect(new Set(result.criticalIssues.map((finding) => finding.id)).size).toBe(2);
  });
});

describe('isValidPublishVersionName', () => {
  it.each(['v1', 'v-1', 'v_1', 'v.1', 'Release2026'])('accepts %s', (name) => {
    expect(isValidPublishVersionName(name)).toBe(true);
  });

  // The server answers 400 with a regex message for each of these.
  it.each(['v 1', 'v/1', 'v#1', 'версия', ''])('refuses %s', (name) => {
    expect(isValidPublishVersionName(name)).toBe(false);
  });
});

describe('PUBLISH_CATEGORIES', () => {
  // The list must be exactly the server's whitelist: a name that is not in
  // `validCategories` is refused with a 422 the author cannot act on.
  it('is the nine names the Go handler accepts', () => {
    expect([...PUBLISH_CATEGORIES]).toEqual([
      'Business Analyst',
      'Quality Assurance',
      'Development',
      'DevOps',
      'Project Management',
      'Knowledge & Documentation',
      'Elitea',
      'Epam',
      'Other',
    ]);
  });
});
