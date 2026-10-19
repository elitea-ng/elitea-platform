# shellcheck shell=sh
#
# Which caller do the LLM checks run as?
#
# Every one of them needs the same answer: a PAT whose user OWNS a personal
# project, because the /llm hop resolves the provider credential from THAT
# project and a caller without one reports `project_not_resolved` — a true
# statement about the wrong caller.
#
# WHY THIS IS A SHARED FUNCTION AND NOT A QUERY REPEATED THREE TIMES.
# standalone-stack.sh and embedding-path-check.sh each
# carried their own copy of that query, and every copy ended
# `ORDER BY t.user_id LIMIT 1`. That named the SEEDED persona only by accident:
# nothing in this stack created a personal project for anybody else, so the
# persona was the only candidate. elitea-main now provisions the caller's
# personal project when it has none
# (services/elitea-main/internal/application/personalproject), and this stack
# triggers it — the #326 edge check calls /social/author as the lowest-id PAT
# owner, dev@elitea.ai. Measured: that gave user 1 a brand-new empty project,
# which then won all three ORDER BYs, and the checks reported "project 2 holds
# no chat model row" while the seeded rows sat in 90003/90106.
#
# Fixing one copy moved the failure to the other two. So the selection lives
# here once, and it selects on the thing the callers are actually about:
# whose personal project HOLDS the seeded chat model row.

# resolve_seeded_driver <reader-fn>
#
# Echoes "<pat-uuid> <project-id>" for the seeded driver, or nothing at all
# when no PAT owns a personal project.
#
# <reader-fn> is the name of a function taking one SQL string and echoing the
# result — each caller already has one, over its own compose project.
#
# The FIRST candidate is kept as the fallback, so a stack whose seed never ran
# still names a project in the caller's own "holds no chat model row" message
# instead of failing against `p_.`.
resolve_seeded_driver() {
    seeded_driver_reader="$1"

    # `</dev/null` on BOTH reads, not only the one inside the loop: the reader
    # runs `compose exec -T`, which inherits whatever stdin the calling script
    # was given and would consume it.
    seeded_driver_candidates="$("$seeded_driver_reader" "SELECT t.uuid || ' ' || p.id
   FROM public.auth_core__token t
   JOIN centry.project p ON p.name = 'project_user_' || t.user_id::text
   JOIN public.auth_core__project_user_role pur
     ON pur.project_id = p.id AND pur.user_id = t.user_id
  WHERE t.uuid IS NOT NULL
  ORDER BY t.user_id" </dev/null)"

    seeded_driver_row=""
    while IFS= read -r seeded_driver_candidate; do
        [ -n "$seeded_driver_candidate" ] || continue
        [ -n "$seeded_driver_row" ] || seeded_driver_row="$seeded_driver_candidate"
        seeded_driver_project="$(printf '%s' "$seeded_driver_candidate" | awk '{print $2}')"
        [ -n "$seeded_driver_project" ] || continue
        # Here `</dev/null` is doubly load-bearing: this loop's stdin is the
        # heredoc below, and a reader that consumed it would swallow the
        # remaining candidates after the first iteration.
        seeded_driver_model="$("$seeded_driver_reader" \
            "SELECT 1 FROM p_${seeded_driver_project}.configuration
              WHERE section = 'llm' AND type = 'llm_model' AND status_ok = true
              LIMIT 1" </dev/null)"
        if [ -n "$(printf '%s' "$seeded_driver_model" | tr -d '[:space:]')" ]; then
            seeded_driver_row="$seeded_driver_candidate"
            break
        fi
    done <<EOF
$seeded_driver_candidates
EOF

    printf '%s' "$seeded_driver_row"
}
