/** Types for `mock-journal-scope.mjs` — see that file for why the rule exists. */
export declare function mockProjectCredential(projectId: string | number): string;
export declare function mockLlmJournalScopeFailure(input: {
  readonly entries: readonly { readonly credential?: unknown }[];
  readonly projectId?: string | number | null;
  readonly host: string;
}): string | undefined;
