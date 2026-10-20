{{- define "s3-local.name" -}}{{ .Release.Name }}-s3{{- end -}}
{{- define "s3-local.labels" -}}
app.kubernetes.io/name: s3-local
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}
{{- define "s3-local.secretName" -}}
{{- default (printf "%s-credentials" (include "s3-local.name" .)) .Values.credentials.existingSecret -}}
{{- end -}}
