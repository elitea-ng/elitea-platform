#!/usr/bin/env bash
# eng-runner job-started hook: the enforcement point for the org runner pool.
#
# The runner runs this script in "Set up runner", BEFORE any step of the job,
# when the runner container has
#   ACTIONS_RUNNER_HOOK_JOB_STARTED=/etc/eng-runner/job-started.sh
# A non-zero exit fails the job before a single step of it executes.
#
# Why here and not in the workflow YAML: a fork pull request runs the FORK's
# workflow files, so it can write `runs-on: eng-runner` itself. YAML routing
# only decides where our own jobs go. This script lives in the cluster (a
# ConfigMap), not in the repository, and reads the event payload GitHub wrote
# (GITHUB_EVENT_PATH), which a workflow cannot forge.
#
# It fails closed: a missing variable, an unreadable payload or an unknown
# event refuses the job.
#
# Uses only what the actions-runner image ships: bash and python3 (no node,
# no jq requirement). Reference copy: elitea-platform deploy/ci-runners/;
# tests: scripts/ci/eng-runner-hook.test.mjs. Runbook: deploy/ci-runners/README.md.
set -euo pipefail

# Repositories allowed on the pool. The override is a file, `allowed-repos` in
# the same read-only ConfigMap (one owner/name per line, `#` comments), never
# an environment variable: depending on the runner version a workflow's
# job-level `env:`, which a fork controls, may reach this process. It is read
# from /etc/eng-runner/allowed-repos (the mount in values-eng-runner.yaml),
# else from next to this script; the first one that exists wins, and the log
# says which list was used.
allowed_repos="elitea-ng/elitea-platform elitea-ng/elitea-docs elitea-ng/elitea-brand-template"
allowlist_source="built-in list"
for override in /etc/eng-runner/allowed-repos "$(dirname -- "$0")/allowed-repos"; do
  [ -e "$override" ] || continue
  [ -r "$override" ] || { echo "::error::eng-runner refuses this job: $override is not readable"; exit 1; }
  allowed_repos="$(sed -e 's/#.*//' -- "$override" | tr -s ' \t\r\n' ' ')"
  allowlist_source="$override"
  break
done

deny() {
  echo "::error title=eng-runner refused this job::eng-runner refuses this job: $1. It is for jobs of our own repositories only; see deploy/ci-runners/README.md in elitea-ng/elitea-platform."
  exit 1
}

repo="${GITHUB_REPOSITORY:-}"
event="${GITHUB_EVENT_NAME:-}"
event_path="${GITHUB_EVENT_PATH:-}"
actor="${GITHUB_ACTOR:-}"
triggering_actor="${GITHUB_TRIGGERING_ACTOR:-}"

[ -n "$repo" ] || deny "GITHUB_REPOSITORY is not set"
[ -n "$event" ] || deny "GITHUB_EVENT_NAME is not set"

case " $allowed_repos " in
  *" $repo "*) ;;
  *) deny "repository $repo is not on the allowlist ($allowlist_source)" ;;
esac

# An allowlist of events, never a denylist: pull_request_target, workflow_run,
# issue_comment, pull_request_review* and anything GitHub adds later run with
# a write token or secrets on behalf of someone who may not have write access.
# In a called (reusable) workflow GITHUB_EVENT_NAME is the caller's event.
# Not merge_group: its payload does not say who wrote the queued change, so a
# Dependabot bump could reach the pool through the queue.
case "$event" in
  pull_request | workflow_dispatch | schedule | push) ;;
  *) deny "event $event is not allowed" ;;
esac

# Dependabot executes freshly bumped third-party code (postinstall, build.rs):
# it stays on GitHub-hosted runners. Both the actor and the re-run actor count.
for who in "$actor" "$triggering_actor"; do
  [ "$who" != "dependabot[bot]" ] || deny "Dependabot job (routed to GitHub-hosted)"
done

if [ "$event" = "pull_request" ]; then
  [ -n "$event_path" ] && [ -r "$event_path" ] || deny "the pull request payload is not readable"
  # Prints "<head repo full name or empty>\t<PR author login or empty>".
  # Exits non-zero on a payload that is not JSON or has no pull_request.
  if ! fields="$(python3 -I - "$event_path" <<'PY'
import json, sys
with open(sys.argv[1], encoding="utf-8") as f:
    pr = json.load(f)["pull_request"]
head = (pr.get("head") or {}).get("repo") or {}
user = pr.get("user") or {}
name = head.get("full_name") or ""
login = user.get("login") or ""
if "\t" in name or "\n" in name or "\t" in login or "\n" in login:
    sys.exit(2)
print(f"{name}\t{login}")
PY
  )"; then
    deny "the pull request payload could not be parsed"
  fi
  head_repo="${fields%%$'\t'*}"
  author="${fields#*$'\t'}"
  # A deleted fork has head.repo == null: refused like any other fork.
  [ "$head_repo" = "$repo" ] || deny "pull request from ${head_repo:-a deleted fork}, not from $repo"
  [ "$author" != "dependabot[bot]" ] || deny "Dependabot pull request (routed to GitHub-hosted)"
fi

echo "eng-runner: accepted $event job of $repo (allowlist: $allowlist_source)"
