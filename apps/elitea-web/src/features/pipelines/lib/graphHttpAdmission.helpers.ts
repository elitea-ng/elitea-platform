import type { AdmissionNode, GraphAdmissionIssue } from './graphAdmission.types';
import { admissionIssue, toStringList } from './graphAdmission.types';

const HTTP_OPERATION_PATTERN = /^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)*$/;
const HTTP_RESPONSE_MODES = new Set(['json', 'text', 'empty']);
const FORBIDDEN_HTTP_NODE_KEYS = ['url', 'method', 'credential', 'credentials', 'auth', 'origin'] as const;

function scalarPreview(value: unknown): string {
  return typeof value === 'string' || typeof value === 'number' || typeof value === 'boolean' ? String(value) : '';
}

function httpResponseMode(raw: unknown): unknown {
  if (raw === null || typeof raw !== 'object' || Array.isArray(raw)) return undefined;
  return (raw as Readonly<Record<string, unknown>>)['mode'];
}

function forbiddenHttpKeyIssues(node: AdmissionNode): readonly GraphAdmissionIssue[] {
  return FORBIDDEN_HTTP_NODE_KEYS
    .filter((forbidden) => Object.hasOwn(node.raw, forbidden))
    .map((forbidden) =>
      admissionIssue(
        'node.required-field',
        'http_action.rs',
        node.id,
        forbidden,
        '',
        `${forbidden}: belongs on the project HTTP action rule, never on the pipeline node.`,
      ),
    );
}

/** `http_action.rs`: revision 2, rule name, request object, response mode, single output key, no URL/method/credentials. */
export function httpRequiredFields(node: AdmissionNode): readonly GraphAdmissionIssue[] {
  const issues: GraphAdmissionIssue[] = [];
  if (node.raw['revision'] !== 2) {
    issues.push(admissionIssue('node.required-field', 'http_action.rs', node.id, 'revision', scalarPreview(node.raw['revision']), 'revision: HTTP Request nodes require revision: 2.'));
  }
  const op = node.raw['operation'];
  if (typeof op !== 'string' || !HTTP_OPERATION_PATTERN.test(op)) {
    issues.push(admissionIssue('node.required-field', 'http_action.rs', node.id, 'operation', scalarPreview(op), 'operation: select a project HTTP action rule (lowercase dotted identifier).'));
  }
  const mode = httpResponseMode(node.raw['response']);
  if (typeof mode !== 'string' || !HTTP_RESPONSE_MODES.has(mode)) {
    issues.push(admissionIssue('node.required-field', 'http_action.rs', node.id, 'response.mode', scalarPreview(mode), 'response.mode: must be json, text, or empty.'));
  }
  const outputs = toStringList(node.raw.output);
  if (outputs.length !== 1) {
    issues.push(admissionIssue('node.required-field', 'http_action.rs', node.id, 'output', outputs.join(', '), 'output: HTTP Request nodes require exactly one output state variable.'));
  }
  return [...issues, ...forbiddenHttpKeyIssues(node)];
}
