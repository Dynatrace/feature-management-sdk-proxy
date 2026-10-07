{{/*
Expand the name of the chart.
*/}}
{{- define "fmsp.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name, truncated to 63 chars (DNS label limit).
*/}}
{{- define "fmsp.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{- define "fmsp.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "fmsp.labels" -}}
helm.sh/chart: {{ include "fmsp.chart" . }}
{{ include "fmsp.selectorLabels" . }}
app.kubernetes.io/version: {{ include "fmsp.imageTag" . | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- with .Values.commonLabels }}
{{ toYaml . }}
{{- end }}
{{- end }}

{{- define "fmsp.selectorLabels" -}}
app.kubernetes.io/name: {{ include "fmsp.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "fmsp.imageTag" -}}
{{- .Values.image.tag | default .Chart.AppVersion }}
{{- end }}

{{/*
Full image reference: registry/repository, then @digest or :tag.
*/}}
{{- define "fmsp.image" -}}
{{- $repo := .Values.image.repository }}
{{- if .Values.image.registry }}
{{- $repo = printf "%s/%s" (trimSuffix "/" .Values.image.registry) $repo }}
{{- end }}
{{- if .Values.image.digest }}
{{- printf "%s@%s" $repo .Values.image.digest }}
{{- else }}
{{- printf "%s:%s" $repo (include "fmsp.imageTag" .) }}
{{- end }}
{{- end }}

{{- define "fmsp.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "fmsp.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Name of the Secret holding static snapshot storage credentials, or empty when
workload identity is used.
*/}}
{{- define "fmsp.credentialsSecretName" -}}
{{- if .Values.snapshot.enabled }}
{{- with .Values.snapshot.credentials }}
{{- if .existingSecret }}
{{- .existingSecret }}
{{- else if .accessKey }}
{{- printf "%s-snapshot" (include "fmsp.fullname" $) }}
{{- end }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Cross-field checks that values.schema.json can't express clearly. Fails at
render time instead of producing a pod that exits 1 at startup.
*/}}
{{- define "fmsp.validate" -}}
{{- with .Values.snapshot }}
{{- if .enabled }}
{{- if or (not .s3.bucket) (not .s3.region) }}
{{- fail "snapshot.s3.bucket and snapshot.s3.region are required when snapshot.enabled=true" }}
{{- end }}
{{- if and .credentials.existingSecret (or .credentials.accessKey .credentials.secretKey) }}
{{- fail "set either snapshot.credentials.existingSecret or snapshot.credentials.accessKey/secretKey, not both" }}
{{- end }}
{{- if ne (empty .credentials.accessKey) (empty .credentials.secretKey) }}
{{- fail "snapshot.credentials.accessKey and snapshot.credentials.secretKey must be set together" }}
{{- end }}
{{- end }}
{{- end }}
{{- end }}
