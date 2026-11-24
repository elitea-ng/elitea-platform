{{- /*
elitea.secretStrength.validate — refuse a weak signing secret written as a
plain env value (#2958).

The services refuse these values at start-up (libs/go/secretstrength): a
blank value (spaces only), a value shorter than 32 bytes after trimming, a
well-known placeholder, or a value that was once committed to this
repository. The chart applies the SAME rule while rendering, so `helm
template` stops with the variable's name instead of a pod that crash-loops.
The message names the variable and never the value.

Only plain env values are read here (main.env, llmGateway.env, deepwiki.env,
inventory.env). A value from a Secret reference (`secrets:`) is not visible
to the chart; the service checks it when it starts. A signing secret belongs
in a Secret anyway: an env value lands in a ConfigMap.

The digests are the SHA-256 of the burned values, the same list as
libs/go/secretstrength/secretstrength.go. Keep the two lists equal.
*/ -}}
{{- define "elitea.secretStrength.burnedDigests" -}}
985fd0d79911300ade63bbcefeb0680eaa75dae04d73139a64fc6b9437a79988
317454db4d4e898e5e733f3a0a524622fc37f801b96f584526d19ebd9450d237
385c622fec13d09f1cb76bdab1efdab2d9cf14bf5600042d149a1b13ea96a1bd
7c1ac23da031fd3ee313f80e3fe48936f487f39c85f6555ce60b7d0395cad710
6516defc8f6b841cdf0e8035416df1e0a0d8009bd76c4a78e1ee9aa772a1728c
0a0ae5942eb01198d8e33da8b8e1443d23c1cae2dd15ae61ad7d43a37854a02d
cedf94ffa5b247c8c8ab51b05408e4e71bf885654ef506c95b71f633359d4b49
30234c5d2d59846ca3b659e7144a0927ea2037fe51c0a68b5302d6370aa1f655
2037a77394d6b1443626c481f7439f8e660b529f968e0129f7cc6a8451ec0701
3ac696c515f9d0b709218abb6c9f369807f47094f5824a552e04bb7bc1837f91
561ff0ece314a9686e04885abdb6cc84125e84562ae6b6be6062b5246163a045
303f3b9691368c44e1185f88d0cff23df77db8152416fc7de195eb18ccf200e8
4daab57012cbe2d10ff2f1fecb78ab4c1fecf245285b0f2725ee857a2234bd2d
953796aa49fa7f48bc24b9b38707a114eb159c6d3766befde0a3c389833891ef
{{- end -}}

{{- /* elitea.secretStrength.check — (dict "where" "main.env" "name" VAR "value" V) */ -}}
{{- define "elitea.secretStrength.check" -}}
{{- $raw := .value | default "" | toString -}}
{{- /* An exactly empty value is "unset", as CheckIfSet reads it. A required
       secret's absence is the service's to refuse: the chart cannot see a
       Secret reference's value. */ -}}
{{- if ne $raw "" -}}
{{- $v := trim $raw -}}
{{- $lower := lower $v -}}
{{- $label := printf "%s.%s" .where .name -}}
{{- $burned := splitList "\n" (include "elitea.secretStrength.burnedDigests" .) -}}
{{- if eq $v "" -}}
{{- fail (printf "%s is empty or blank. Leave it unset, or put a random value of at least 32 bytes in a Secret and reference it (for example: openssl rand -base64 32)." $label) -}}
{{- end -}}
{{- if has $lower (list "changeme" "change-me" "change_me" "replace-me" "replaceme" "secret" "password" "elitea" "placeholder" "example") -}}
{{- fail (printf "%s is a well-known placeholder. Generate a random value of at least 32 bytes (for example: openssl rand -base64 32) and reference it from a Secret." $label) -}}
{{- end -}}
{{- range list "changeme" "change-me" "change_me" "replace-me" "replaceme" "placeholder" -}}
{{- if contains . $lower -}}
{{- fail (printf "%s contains a placeholder marker. Generate a random value of at least 32 bytes (for example: openssl rand -base64 32) and reference it from a Secret." $label) -}}
{{- end -}}
{{- end -}}
{{- if or (has (sha256sum $raw) $burned) (has (sha256sum $v) $burned) -}}
{{- fail (printf "%s holds a value that was published in this repository and is burned. Generate a new random value (for example: openssl rand -base64 32) and reference it from a Secret." $label) -}}
{{- end -}}
{{- if lt (len $v) 32 -}}
{{- fail (printf "%s is shorter than 32 bytes. Generate a random value of at least 32 bytes (for example: openssl rand -base64 32) and reference it from a Secret." $label) -}}
{{- end -}}
{{- if lt (len (uniq (splitList "" $v))) 8 -}}
{{- fail (printf "%s repeats too few characters to be random (fewer than 8 different ones). Generate a random value (for example: openssl rand -base64 32) and reference it from a Secret." $label) -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- /* elitea.secretStrength.requireIdentity — (dict "Values" .Values "component" "deepwiki" "var" "ELITEA_DEEPWIKI_IDENTITY_SECRET")
       An enabled provider needs its identity secret on BOTH sides: elitea-main
       refuses to start without it, and the host refuses every call (#2959).
       A side has it when its `secrets:` map references a Secret, or its `env:`
       map holds a value. */ -}}
{{- define "elitea.secretStrength.requireIdentity" -}}
{{- $component := get .Values .component | default dict -}}
{{- if $component.enabled -}}
{{- range $side := list (dict "where" "main" "section" (.Values.main | default dict)) (dict "where" .component "section" $component) -}}
{{- $ref := get ($side.section.secrets | default dict) $.var -}}
{{- $env := get ($side.section.env | default dict) $.var -}}
{{- if and (not (and $ref $ref.secretName $ref.key)) (not $env) -}}
{{- fail (printf "%s.enabled needs %s on %s: set %s.secrets.%s.secretName / .key to a Secret holding at least 32 random bytes (for example: openssl rand -base64 32). elitea-main and the provider must carry the same value." $.component $.var $side.where $side.where $.var) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "elitea.secretStrength.validate" -}}
{{- include "elitea.secretStrength.requireIdentity" (dict "Values" .Values "component" "deepwiki" "var" "ELITEA_DEEPWIKI_IDENTITY_SECRET") -}}
{{- include "elitea.secretStrength.requireIdentity" (dict "Values" .Values "component" "inventory" "var" "ELITEA_INVENTORY_IDENTITY_SECRET") -}}
{{- $names := list "APPLICATION_SECRET_KEY" "GATEWAY_IDENTITY_SECRET" "GATEWAY_HOP_SECRET" "ELITEA_DEEPWIKI_IDENTITY_SECRET" "ELITEA_INVENTORY_IDENTITY_SECRET" -}}
{{- range $where, $section := dict "main.env" (.Values.main | default dict) "llmGateway.env" (.Values.llmGateway | default dict) "deepwiki.env" (.Values.deepwiki | default dict) "inventory.env" (.Values.inventory | default dict) -}}
{{- $env := get $section "env" | default dict -}}
{{- range $names -}}
{{- if hasKey $env . -}}
{{- include "elitea.secretStrength.check" (dict "where" $where "name" . "value" (get $env .)) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
