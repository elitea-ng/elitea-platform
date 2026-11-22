#!/bin/sh
# The bootstrap identities' issuer: the init container of the elitea-nats-bootstrap
# Job (templates/natsBootstrap/job.yaml, issuer.yaml).
#
# For each account it creates the account's cert-manager Certificate, owned by
# this Job, waits until cert-manager reports it Ready, and copies the Secret's
# ca.crt, tls.crt and tls.key into OUT_DIR/<account> for the bootstrap
# container.
#
# Why a container creates them, and the chart does not render them: Argo CD
# gates a hook on its health, and its health check for a Certificate reads
# Ready=False as Degraded. cert-manager's readiness and issuing controllers are
# separate, so a new Certificate can carry Ready=False (reason DoesNotExist) for
# a moment before Issuing=True. Argo CD then failed the sync, although the
# certificate was issued a second later. Here nothing reads health: this
# script waits for Ready=True and ignores every state before it.
#
# Lifecycle: the ownerReference names this Job, so the garbage collector deletes
# the Certificates with the Job (Helm and Argo CD both delete it once it
# succeeds, and before the next run), and cert-manager never renews them. Each
# run also starts from the state of a first install (#1370): a Certificate it
# does not own is deleted, and waited for, BEFORE its Secret is deleted (a live
# Certificate would write the Secret straight back), and only then is the new
# Certificate created.
#
# Inputs (env):
#   ACCOUNTS         the accounts, space-separated
#   CERT_DIR         holds certificate-<account>.json (the rendered Certificates)
#   OUT_DIR          the bootstrap's NATS_TLS_DIR (an in-memory emptyDir)
#   WORK_DIR         a writable scratch directory
#   JOB_NAME         this Job's name
#   JOB_UID          this Job's uid (the pod's batch.kubernetes.io/controller-uid
#                    label); JOB_UID_LEGACY (controller-uid) if that is empty
#   TOKEN_DIR        the projected token, ca.crt and namespace
#   ISSUE_WAIT       seconds to wait for every Certificate to be Ready
set -eu

log() { echo "nats-bootstrap-issue: $*"; }
fail() { echo "nats-bootstrap-issue: FAILED: $*" >&2; exit 1; }

uid="${JOB_UID:-}"
[ -n "$uid" ] || uid="${JOB_UID_LEGACY:-}"
[ -n "$uid" ] || fail "the pod carries no controller-uid label, so the Certificates cannot name this Job as their owner"

ns=$(cat "$TOKEN_DIR/namespace")
core="https://kubernetes.default.svc/api/v1/namespaces/$ns/secrets"
certs="https://kubernetes.default.svc/apis/cert-manager.io/v1/namespaces/$ns/certificates"
body="$WORK_DIR/body"
umask 077

# call METHOD URL [FILE] prints the HTTP status and leaves the response in
# $body. The token goes to curl on stdin, not in argv; it is re-read on every
# call because the kubelet rotates a projected token. A request that never got
# an answer prints 000 (curl names the error on stderr), so the caller still
# reports which call it was.
call() {
  rm -f "$body"
  if [ -n "${3:-}" ]; then
    printf 'Authorization: Bearer %s\n' "$(cat "$TOKEN_DIR/token")" \
      | curl -sS --cacert "$TOKEN_DIR/ca.crt" -H @- -H 'Content-Type: application/json' \
          -o "$body" -w '%{http_code}' -X "$1" --data-binary "@$3" "$2" || true
  else
    printf 'Authorization: Bearer %s\n' "$(cat "$TOKEN_DIR/token")" \
      | curl -sS --cacert "$TOKEN_DIR/ca.crt" -H @- \
          -o "$body" -w '%{http_code}' -X "$1" "$2" || true
  fi
}

# The API's reason for a refusal, for the log.
why() { jq -r '.message // empty' "$body" 2>/dev/null || true; }

# gone URL WHAT waits until GET URL answers 404.
gone() {
  i=0
  while :; do
    code=$(call GET "$1")
    [ "$code" = 404 ] && return 0
    i=$((i + 1))
    [ "$i" -lt 60 ] || fail "$2 is still present after 60s (GET answered HTTP $code: $(why))"
    sleep 1
  done
}

# delete URL WHAT deletes and waits until it is gone.
delete() {
  code=$(call DELETE "$1")
  case "$code" in
    200|202) log "deleted $2" ;;
    404) return 0 ;;
    *) fail "DELETE $2 answered HTTP $code: $(why)" ;;
  esac
  gone "$1" "$2"
}

for acct in $ACCOUNTS; do
  src="$CERT_DIR/certificate-$acct.json"
  [ -r "$src" ] || fail "no rendered Certificate for account $acct ($src)"
  name=$(jq -r '.metadata.name' "$src")
  secret=$(jq -r '.spec.secretName' "$src")

  code=$(call GET "$certs/$name")
  case "$code" in
    200)
      if [ "$(jq -r --arg uid "$uid" '[.metadata.ownerReferences[]? | select(.uid == $uid)] | length' "$body")" != 0 ]; then
        # A restarted pod of this same Job: its Certificate stands.
        log "Certificate $name is this Job's already"
        continue
      fi
      # Left by an earlier run (or an earlier chart, whose Certificates were
      # hooks). Delete it first, so it cannot re-write the Secret.
      delete "$certs/$name" "the Certificate $name of an earlier run"
      ;;
    404) ;;
    *) fail "GET Certificate $name answered HTTP $code: $(why)" ;;
  esac

  delete "$core/$secret" "the Secret $secret of an earlier run"

  jq --arg job "$JOB_NAME" --arg uid "$uid" \
    '.metadata.ownerReferences = [{apiVersion: "batch/v1", kind: "Job", name: $job, uid: $uid}]' \
    "$src" > "$WORK_DIR/create.json"
  code=$(call POST "$certs" "$WORK_DIR/create.json")
  case "$code" in
    201) log "created the Certificate $name" ;;
    *) fail "POST Certificate $name answered HTTP $code: $(why)" ;;
  esac
done

# Wait for Ready=True, at this generation. Every earlier state — no status,
# Ready=False/DoesNotExist, Issuing=True — is a certificate on its way.
deadline=$(( $(date +%s) + ISSUE_WAIT ))
for acct in $ACCOUNTS; do
  name=$(jq -r '.metadata.name' "$CERT_DIR/certificate-$acct.json")
  while :; do
    code=$(call GET "$certs/$name")
    [ "$code" = 200 ] || fail "GET Certificate $name answered HTTP $code: $(why)"
    ready=$(jq -r '. as $c | [.status.conditions[]? | select(.type == "Ready" and .status == "True" and ((.observedGeneration // $c.metadata.generation) == $c.metadata.generation))] | length' "$body")
    [ "$ready" = 0 ] || break
    if [ "$(date +%s)" -ge "$deadline" ]; then
      state=$(jq -r '[.status.conditions[]? | "\(.type)=\(.status) (\(.reason // "-")): \(.message // "")"] | join("; ")' "$body")
      fail "the Certificate $name is not Ready after ${ISSUE_WAIT}s: ${state:-no status yet}"
    fi
    sleep 2
  done
  log "the Certificate $name is Ready"
done

for acct in $ACCOUNTS; do
  secret=$(jq -r '.spec.secretName' "$CERT_DIR/certificate-$acct.json")
  code=$(call GET "$core/$secret")
  [ "$code" = 200 ] || fail "GET Secret $secret answered HTTP $code: $(why)"
  mkdir -p "$OUT_DIR/$acct"
  for k in ca.crt tls.crt tls.key; do
    v=$(jq -r --arg k "$k" '.data[$k] // empty' "$body")
    [ -n "$v" ] || fail "the Secret $secret has no $k"
    printf '%s' "$v" | base64 -d > "$OUT_DIR/$acct/$k"
  done
  log "copied the $acct identity"
done
rm -f "$body" "$WORK_DIR/create.json"
log "done"
