{{- define "sbx.labels" -}}
app.kubernetes.io/part-of: sbx
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/* sbx.image renders registry/repository:tag for one of .Values.images. */}}
{{- define "sbx.image" -}}
{{- $root := .root -}}
{{- $tag := default $root.Values.images.tag .image.tag -}}
{{- if not $tag }}{{ fail (printf "set images.tag or images.%s.tag" .name) }}{{ end -}}
{{- with $root.Values.images.registry }}{{ . }}/{{ end }}{{ .image.repository }}:{{ $tag }}
{{- end -}}

{{- define "sbx.signingSecret" -}}
{{- default (printf "%s-master-key" .Release.Name) .Values.signing.existingSecret -}}
{{- end -}}

{{- define "sbx.s3Endpoint" -}}
{{- if .Values.s3.endpoint }}{{ .Values.s3.endpoint }}
{{- else if .Values.s3local.enabled }}http://{{ .Release.Name }}-s3.{{ .Release.Namespace }}.svc:8333
{{- else }}{{ fail "set s3.endpoint (or enable s3local)" }}{{ end -}}
{{- end -}}

{{- define "sbx.s3Secret" -}}
{{- if .Values.s3.existingSecret }}{{ .Values.s3.existingSecret }}
{{- else if .Values.s3local.enabled }}{{ .Release.Name }}-s3-credentials
{{- else }}{{ fail "set s3.existingSecret (or enable s3local)" }}{{ end -}}
{{- end -}}

{{- define "sbx.brokerAddr" -}}
{{ .Release.Name }}-controller.{{ .Release.Namespace }}.svc:{{ .Values.controller.brokerPort }}
{{- end -}}
