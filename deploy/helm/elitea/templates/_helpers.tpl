{{/*
Cross-component helpers. Everything here answers a question more than one
component asks, which is the same rule that decides what lives at the top of
values.yaml.
*/}}

{{/*
An in-cluster service address, namespace-relative by default.

`namespace: ""` means THIS release's namespace, which is the only default a
chart installable into any namespace can honestly have. A hardcoded namespace
in a shipped default is worse than a missing one: installing into `elitea-prod`
while the default still says `.elitea.svc` silently points the new release at
another environment's NATS — sharing its budget counters and rate
limits — and nothing reports it as a misconfiguration.

Usage: {{ include "elitea.serviceHost" (dict "svc" .Values.nats "ctx" .) }}
*/}}
{{- define "elitea.serviceHost" -}}
{{- $svc := .svc -}}
{{- $ns := $svc.namespace | default .ctx.Release.Namespace -}}
{{- printf "%s.%s.svc.cluster.local" $svc.service $ns -}}
{{- end }}

{{- define "elitea.serviceAddr" -}}
{{- $svc := .svc -}}
{{- printf "%s:%v" (include "elitea.serviceHost" .) $svc.port -}}
{{- end }}

{{/*
elitea.eventsNatsUrl — the NATS URL elitea-main's live-update plane
(ELITEA_EVENTS_NATS_URL) gets when main.env does not state one: the SAME
effective NATS the LLM gateway uses, because the gateway publishes
budget.soft_alert onto the subject the project SSE stream reads. Deriving it
from the top-level `nats` block alone, while the gateway honours an explicit
llmGateway.env.GATEWAY_NATS_URL, sent main to a host that may not exist
(CrashLoop: a configured but unreachable NATS stops elitea-main) and split
soft alerts onto a broker nobody streams from.

Order: llmGateway.env.GATEWAY_NATS_URL (gateway enabled), then
scheduler.env.GATEWAY_NATS_URL (scheduler enabled), then the `nats` block.
Empty when none names a NATS — elitea-main then runs with no live-update plane.

A GATEWAY_NATS_URL the gateway reads from a Secret (llmGateway.secrets) is not
visible here, so elitea-main.validateEventsNats refuses that shape unless main
names its own URL too.
*/}}
{{- define "elitea.eventsNatsUrl" -}}
{{- $gw := .Values.llmGateway.env | default dict -}}
{{- $sched := .Values.scheduler.env | default dict -}}
{{- if and .Values.llmGateway.enabled (get $gw "GATEWAY_NATS_URL") -}}
{{- get $gw "GATEWAY_NATS_URL" -}}
{{- else if and .Values.scheduler.enabled (get $sched "GATEWAY_NATS_URL") -}}
{{- get $sched "GATEWAY_NATS_URL" -}}
{{- else if .Values.nats.service -}}
{{- include "elitea.natsUrl" . -}}
{{- end -}}
{{- end }}

{{/*
elitea.natsUrl — the URL every component gets from the top-level `nats` block:
tls:// when nats.tls.enabled (the NATS chart requires TLS, #1076), nats://
only in the acknowledged plaintext posture. Never a credential.
*/}}
{{- define "elitea.natsUrl" -}}
{{- printf "%s://%s" (ternary "tls" "nats" (.Values.nats.tls.enabled | default false)) (include "elitea.serviceAddr" (dict "svc" .Values.nats "ctx" .)) -}}
{{- end }}

{{/*
elitea.natsClientFile — one file of the mounted NATS client certificate
(ca.crt, tls.crt or tls.key). Each component writes its three
<PREFIX>_NATS_TLS_*_FILE names literally in its own template. The code does
NOT name them literally: libs/go/natsconn builds them by concatenation
(EnvNames(prefix)), which the env-drift gate
(services/elitea-llm-gateway/scripts/env-drift-check.sh) cannot see. So
deploy/helm/tests/render-nats-security.sh reads every prefix the code passes
to natsconn and asserts the rendered chart sets all three names for it.
*/}}
{{- define "elitea.natsClientFile" -}}
{{- printf "%s/%s" (trimSuffix "/" .ctx.Values.nats.tls.mountPath) .file | quote -}}
{{- end }}

{{/*
elitea.natsClientVolumeMount / elitea.natsClientVolume — the client
certificate Secret (tls.crt, tls.key, ca.crt) cert-manager writes for one
component. A whole-directory mount: natsconn reads the files with
tls.LoadX509KeyPair / os.ReadFile on every handshake, so the Secret volume's
symlink swap on renewal is exactly what it should see.
*/}}
{{- define "elitea.natsClientVolumeMount" -}}
- name: nats-client-tls
  mountPath: {{ .Values.nats.tls.mountPath }}
  readOnly: true
{{- end }}

{{- define "elitea.natsClientVolume" -}}
- name: nats-client-tls
  secret:
    secretName: {{ .secretName }}
{{- end }}

{{/*
elitea-main.usesNats — "true" when elitea-main dials NATS at all: a URL from
its own env, from a Secret, or derived from the gateway / the `nats` block.
*/}}
{{- define "elitea-main.usesNats" -}}
{{- $env := .Values.main.env | default dict -}}
{{- if or (get $env "ELITEA_EVENTS_NATS_URL") (hasKey (.Values.main.secrets | default dict) "ELITEA_EVENTS_NATS_URL") (include "elitea.eventsNatsUrl" .) -}}
true
{{- end -}}
{{- end }}

{{/*
Image pull secrets, applied to every component's pod spec.

Defined here rather than per component because a private registry is a property
of the deployment, not of one service — and because a pull-secret setting that
is declared and read by nothing produces ImagePullBackOff on every pod while
the values file claims it is applied.
*/}}
{{- define "elitea.imagePullSecrets" -}}
{{- $all := concat (.local | default list) (.global | default list) -}}
{{- if $all }}
imagePullSecrets:
{{- range $all }}
  - name: {{ .name }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Chart-level labels, for resources that belong to the release rather than to any
one component (the database bootstrap, the guards). Component resources keep
their own <component>.labels helpers, so the labels an existing object carries
do not change.
*/}}
{{- define "elitea.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{ include "elitea.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "elitea.selectorLabels" -}}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
elitea.aiProjectId — the ONE id of the platform's public ("shared") project.

# THE PROBLEM

The same project was named by four keys across this chart, and nothing checked
that they agreed:

  * `main.env.ELITEA_AI_PROJECT_ID` — the project whose configurations are
    public, and the schema the admin provider surface WRITES a shared
    credential into.
  * `llmGateway.env.ELITEA_AI_PROJECT_ID` — the schema the gateway RESOLVES a
    shared credential out of.
  * `web.env.VITE_PUBLIC_PROJECT_ID` — the id the SPA compares the selected
    project against.

Two different values do not produce an error. The credential is stored, listed,
reported healthy — and resolves for nobody. The SPA offers the shared models the
gateway cannot find. Every probe stays green, so the operator learns about it
from a user whose chat turn failed.

`deploy/helm/tests/render-llm-path.sh` compared the first two, over two
renders, and only because they used to be separate charts. They are one chart
now, so a template can see all three, and a check that runs at `helm template`
time runs for every operator rather than only in this repository's CI.

# THE RULE

Set `platform.aiProjectId` ONCE. Every component below takes it.

A values file that still sets a component key keeps working, as long as the
values AGREE — that is the whole backward-compatibility contract here, and it
covers every values file in this repository. Two that disagree stop the render
with the message below, which costs the operator one message instead of a
debugging session.

Returns "" when nothing names a project. "" is a real state, not a missing one:
with the Configurations plane off, elitea-main REFUSES to start when
ELITEA_AI_PROJECT_ID is present at all
(cmd/elitea-main/configurations_config.go), so this helper must never invent a
value for a deployment that did not ask for one.
*/}}
{{- define "elitea.aiProjectId" -}}
{{- $sources := list
      (dict "key" "platform.aiProjectId"                "value" ((.Values.platform | default dict).aiProjectId | default "" | toString))
      (dict "key" "main.env.ELITEA_AI_PROJECT_ID"       "value" (get (.Values.main.env | default dict) "ELITEA_AI_PROJECT_ID" | default "" | toString))
      (dict "key" "llmGateway.env.ELITEA_AI_PROJECT_ID" "value" (get (.Values.llmGateway.env | default dict) "ELITEA_AI_PROJECT_ID" | default "" | toString))
      (dict "key" "web.env.VITE_PUBLIC_PROJECT_ID"      "value" (get (.Values.web.env | default dict) "VITE_PUBLIC_PROJECT_ID" | default "" | toString))
-}}
{{- $chosen := "" -}}
{{- $chosenKey := "" -}}
{{- range $source := $sources -}}
{{- if $source.value -}}
{{- if not (regexMatch "^[1-9][0-9]*$" $source.value) -}}
{{- fail (printf "%s must be a positive project id, got %q. It becomes the PostgreSQL schema name p_<id>, so a value that is not an id is a schema that does not exist and every shared-credential read fails." $source.key $source.value) -}}
{{- end -}}
{{- if not $chosen -}}
{{- $chosen = $source.value -}}
{{- $chosenKey = $source.key -}}
{{- else if ne $chosen $source.value -}}
{{- fail (printf "%s=%s and %s=%s name different public projects, and they are ONE setting. elitea-main WRITES a shared credential into p_<id>, the LLM gateway RESOLVES it out of p_<id>, and the SPA decides which project is public with a third copy of the same id — so a disagreement stores a credential nobody can resolve, with every pod Ready and nothing logged. Set platform.aiProjectId once and remove the component keys." $chosenKey $chosen $source.key $source.value) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- $chosen -}}
{{- end }}
