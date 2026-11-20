{{/*
kairon.ui.usersConfigured returns "true" when either ui.auth.users or
ui.auth.existingSecret names a real username/password login account
source. Shared between all.yaml and NOTES.txt so they can never disagree
about whether real login accounts are configured.
*/}}
{{- define "kairon.ui.usersConfigured" -}}
{{- if or (gt (len .Values.ui.auth.users) 0) .Values.ui.auth.existingSecret -}}true{{- end -}}
{{- end -}}

{{/*
kairon.ui.anyAuthConfigured returns "true" when kairon-ui has any explicit
auth configured: a shared token, a pre-created token Secret, real
username/password accounts, or an explicit unauthenticated opt-in. When
this is false, the chart seeds a default "admin" account instead of
refusing to install (see all.yaml / NOTES.txt).
*/}}
{{- define "kairon.ui.anyAuthConfigured" -}}
{{- if or .Values.ui.allowUnauthenticated .Values.ui.token .Values.ui.existingSecret (eq (include "kairon.ui.usersConfigured" .) "true") -}}true{{- end -}}
{{- end -}}

{{/*
kairon.podMeta renders the remainder of a pod template's `metadata:` line.
With neither podAnnotations nor podLabels set it emits exactly the original
one-line flow form (default render is unchanged); otherwise a block with the
extra labels/annotations. The selector label app.kubernetes.io/name is always
set last so podLabels can never override it.
Args: dict "name" <app name> "v" <workload values>
*/}}
{{- define "kairon.podMeta" -}}
{{- if or .v.podAnnotations .v.podLabels -}}
{{- $labels := merge (dict "app.kubernetes.io/name" .name) (default (dict) .v.podLabels) -}}
{{- $_ := set $labels "app.kubernetes.io/name" .name }}
{{- with .v.podAnnotations }}
      annotations:
{{ toYaml . | indent 8 }}
{{- end }}
      labels:
{{ toYaml $labels | indent 8 }}
{{- else }} {labels: {app.kubernetes.io/name: {{ .name }}}}
{{- end -}}
{{- end -}}

{{/*
kairon.podSpecExtras renders optional pod-spec scheduling keys (all off by
default): priorityClassName, terminationGracePeriodSeconds and
topologySpreadConstraints. A constraint without a labelSelector gets the
pod's own selector label. Emits nothing when none are set. Callers use it as
`{{ with include "kairon.podSpecExtras" (dict "name" N "v" V) }}{{ . | nindent 6 }}{{ end }}`.
Args: dict "name" <app name> "v" <workload values>
*/}}
{{- define "kairon.podSpecExtras" -}}
{{- $name := .name -}}
{{- if .v.priorityClassName }}
priorityClassName: {{ .v.priorityClassName | quote }}
{{- end }}
{{- if not (kindIs "invalid" .v.terminationGracePeriodSeconds) }}
terminationGracePeriodSeconds: {{ .v.terminationGracePeriodSeconds | int64 }}
{{- end }}
{{- if .v.topologySpreadConstraints }}
topologySpreadConstraints:
{{- range .v.topologySpreadConstraints }}
{{- $c := deepCopy . }}
{{- if not (hasKey $c "labelSelector") }}
{{- $_ := set $c "labelSelector" (dict "matchLabels" (dict "app.kubernetes.io/name" $name)) }}
{{- end }}
{{ toYaml (list $c) | indent 2 }}
{{- end }}
{{- end }}
{{- end -}}
