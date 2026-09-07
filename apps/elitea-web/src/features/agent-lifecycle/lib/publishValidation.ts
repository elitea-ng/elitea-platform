import type { PublishRequest, PublishValidationResult, ValidationIssue } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';

/**
 * The publish wizard's Validation step, reduced to what a reader can act on.
 *
 * `runPublishValidation` (internal/api/v2/eliteacore/handler.go) answers ONE
 * body for three different situations, and the differences matter:
 *
 *  - `validateForPublish` answers **200** on PASS/WARN and **422** on FAIL,
 *    with the SAME `PublishValidationResult` body. A 422 here is therefore a
 *    result, not a transport failure, and the wizard must render it rather
 *    than report "the request failed".
 *  - Every issue key is optional by construction, and the three lists carry
 *    their text under three DIFFERENT keys: critical issues use `issue` (plus
 *    `fix`), the publish 422 short-circuit reshapes them to `message`, and
 *    recommendations carry `suggestion` and no `issue` at all. Reading only
 *    `issue` renders an empty bullet for a whole class of finding.
 *  - `validation_token` is null on FAIL. Only a non-FAIL run yields a token,
 *    and only a token lets the publish call skip re-validating.
 */

type PublishValidationStatus = 'PASS' | 'WARN' | 'FAIL';

export interface PublishValidationFinding {
  readonly id: string;
  readonly text: string;
  /** Remediation text, when the rule carries one. */
  readonly fix?: string;
}

export interface NormalizedPublishValidation {
  readonly status: PublishValidationStatus;
  readonly criticalIssues: readonly PublishValidationFinding[];
  readonly warnings: readonly PublishValidationFinding[];
  readonly recommendations: readonly PublishValidationFinding[];
  /** Present only when the run did not FAIL. */
  readonly validationToken?: string;
  readonly summary?: string;
}

/**
 * The one place that knows an issue can name itself four ways. Order matters:
 * `issue` is the server's own field, `message` the publish-422 reshape,
 * `suggestion` the recommendations-only field, `rule` the last resort so a
 * finding is never rendered as an empty line.
 */
function findingText(issue: ValidationIssue): string {
  return (
    issue.issue ??
    issue.message ??
    issue.suggestion ??
    issue.rule ??
    t('features.agentLifecycle.publish.unnamedIssue', 'Unnamed validation finding.')
  );
}

function toFindings(issues: readonly ValidationIssue[] | undefined, prefix: string): readonly PublishValidationFinding[] {
  if (issues === undefined) return [];
  return issues.map((issue, index) => ({
    id: `${prefix}-${issue.rule ?? String(index)}-${String(index)}`,
    text: findingText(issue),
    ...(issue.fix === undefined ? {} : { fix: issue.fix }),
  }));
}

/**
 * `status` is optional in the schema — the deterministic short-circuit 422s
 * carry `issues` and nothing else. An absent status with findings present is
 * a FAIL, and an absent status with nothing present is a PASS; treating an
 * absent status as PASS in both cases would let the wizard walk past a
 * refusal it was just handed.
 */
function deriveStatus(
  declared: PublishValidationResult['status'],
  criticalCount: number,
  warningCount: number,
): PublishValidationStatus {
  if (declared !== undefined) return declared;
  if (criticalCount > 0) return 'FAIL';
  return warningCount > 0 ? 'WARN' : 'PASS';
}

/** A token is only usable when the run did not FAIL; the server sends null there. */
function usableToken(status: PublishValidationStatus, token: string | null | undefined): string | undefined {
  if (status === 'FAIL') return undefined;
  return token === null || token === undefined || token === '' ? undefined : token;
}

export function normalizePublishValidation(result: PublishValidationResult | undefined): NormalizedPublishValidation {
  const criticalIssues = toFindings(result?.critical_issues ?? result?.issues, 'critical');
  const warnings = toFindings(result?.warnings, 'warning');
  const recommendations = toFindings(result?.recommendations, 'recommendation');
  const status = deriveStatus(result?.status, criticalIssues.length, warnings.length);
  const token = usableToken(status, result?.validation_token);
  return {
    status,
    criticalIssues,
    warnings,
    recommendations,
    ...(token === undefined ? {} : { validationToken: token }),
    ...(result?.summary === undefined ? {} : { summary: result.summary }),
  };
}

/**
 * The nine categories the Go publish handler accepts (handler.go
 * `validCategories`), typed from the generated request so a tenth added to the
 * spec and not to this list fails the build instead of reaching a 422.
 */
export type PublishCategory = NonNullable<PublishRequest['category']>;

export const PUBLISH_CATEGORIES: readonly PublishCategory[] = [
  'Business Analyst',
  'Quality Assurance',
  'Development',
  'DevOps',
  'Project Management',
  'Knowledge & Documentation',
  'Elitea',
  'Epam',
  'Other',
];

/** The server's own rule: `^[a-zA-Z0-9._-]+$`, refused with a 400 otherwise. */
export function isValidPublishVersionName(name: string): boolean {
  return /^[a-zA-Z0-9._-]+$/.test(name);
}
