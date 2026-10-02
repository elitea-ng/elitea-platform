import { z } from 'zod';

/**
 * `applicationCreationSchema` — ported from
 * `apps/elitea-ui/src/pages/Applications/Components/Applications/ApplicationCreationValidateSchema.js`
 * (Wave-2 promotion pass; see the `entities/application-form` module doc in
 * `index.ts` for why this lives in its own slice rather than
 * `entities/application`).
 *
 * The baseline used `yup` (no longer a dependency of this app — see
 * `package.json`); this is a faithful `zod` re-expression of the exact same
 * validation rules, not a redesign:
 *  1. `name` is required (non-blank).
 *  2. `description` is required (non-blank).
 *  3. Each `version_details.conversation_starters[]` entry must be either
 *     absent (`undefined`/`null`) or a non-blank string — the baseline's own
 *     test predicate, `value === undefined || value === null ||
 *     value.trim().length > 0`, reproduced verbatim via `.refine` (not
 *     `.trim().min(1)`, which would also silently coerce/transform the
 *     value — this schema only validates, exactly like the baseline).
 *  4. Version tags are strings. Zod removes undeclared object keys before
 *     the submit callback, so the field must be part of this schema or a
 *     visible tag edit is silently lost on Save.
 */
const conversationStarterEntrySchema = z
  .string()
  .nullable()
  .optional()
  .refine((value) => value === undefined || value === null || value.trim().length > 0, {
    message: 'Chat starter cannot be empty',
  });

export const applicationCreationSchema = z.object({
  name: z.string().min(1, 'Name is required'),
  description: z.string().min(1, 'Description is required'),
  version_details: z
    .object({
      conversation_starters: z.array(conversationStarterEntrySchema).optional(),
      tags: z.array(z.string()).optional(),
    })
    .optional(),
});

export type ApplicationCreationInput = z.infer<typeof applicationCreationSchema>;
