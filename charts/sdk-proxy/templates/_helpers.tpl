{{/*
Expand the name of the chart.
*/}}
{{- define "sdk-proxy.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name, truncated to 63 chars (DNS label limit).
*/}}
{{- define "sdk-proxy.fullname" -}}
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

{{- define "sdk-proxy.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "sdk-proxy.labels" -}}
helm.sh/chart: {{ include "sdk-proxy.chart" . }}
{{ include "sdk-proxy.selectorLabels" . }}
app.kubernetes.io/version: {{ include "sdk-proxy.imageTag" . | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "sdk-proxy.selectorLabels" -}}
app.kubernetes.io/name: {{ include "sdk-proxy.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Image tag. Release images are tagged with the git tag (v-prefixed), so the
default is "v" + appVersion.
*/}}
{{- define "sdk-proxy.imageTag" -}}
{{- .Values.image.tag | default (printf "v%s" .Chart.AppVersion) }}
{{- end }}

{{- define "sdk-proxy.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "sdk-proxy.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Name of the Secret holding static S3 credentials, or empty if none are configured.
*/}}
{{- define "sdk-proxy.s3SecretName" -}}
{{- if .Values.snapshot.s3.existingSecret }}
{{- .Values.snapshot.s3.existingSecret }}
{{- else if or .Values.snapshot.s3.accessKey .Values.snapshot.s3.secretKey }}
{{- printf "%s-s3" (include "sdk-proxy.fullname" .) }}
{{- end }}
{{- end }}

{{- define "sdk-proxy.snapshotClaimName" -}}
{{- default (printf "%s-snapshots" (include "sdk-proxy.fullname" .)) .Values.snapshot.local.persistence.existingClaim }}
{{- end }}

{{/*
Fail early on configurations the proxy would reject at startup (exit 1).
*/}}
{{- define "sdk-proxy.validate" -}}
{{- $backend := .Values.snapshot.backend }}
{{- if not (has $backend (list "" "s3" "local")) }}
{{- fail (printf "snapshot.backend must be \"\", \"s3\", or \"local\" (got %q)" $backend) }}
{{- end }}
{{- if eq $backend "s3" }}
{{- if or (not .Values.snapshot.s3.bucket) (not .Values.snapshot.s3.region) }}
{{- fail "snapshot.s3.bucket and snapshot.s3.region are required when snapshot.backend=s3" }}
{{- end }}
{{- if and (not .Values.snapshot.s3.existingSecret) (ne (empty .Values.snapshot.s3.accessKey) (empty .Values.snapshot.s3.secretKey)) }}
{{- fail "snapshot.s3.accessKey and snapshot.s3.secretKey must be set together" }}
{{- end }}
{{- end }}
{{- if eq $backend "local" }}
{{- $multi := or .Values.autoscaling.enabled (gt (int .Values.replicaCount) 1) }}
{{- if and $multi (not .Values.snapshot.local.persistence.existingClaim) (not (has "ReadWriteMany" .Values.snapshot.local.persistence.accessModes)) }}
{{- fail "snapshot.backend=local with more than one replica requires a ReadWriteMany volume (or an existingClaim); use snapshot.backend=s3 for multi-replica deployments" }}
{{- end }}
{{- end }}
{{- range $k, $_ := .Values.config }}
{{- if has $k (list "PORT" "PROXY_INSTANCE_ID") }}
{{- fail (printf "config.%s is managed by the chart; use containerPort instead of PORT" $k) }}
{{- end }}
{{- end }}
{{- end }}
