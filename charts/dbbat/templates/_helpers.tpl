{{/*
Expand the name of the chart.
*/}}
{{- define "dbbat.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
If release name contains chart name it will be used as a full name.
*/}}
{{- define "dbbat.fullname" -}}
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

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "dbbat.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "dbbat.labels" -}}
helm.sh/chart: {{ include "dbbat.chart" . }}
{{ include "dbbat.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "dbbat.selectorLabels" -}}
app.kubernetes.io/name: {{ include "dbbat.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Create the name of the service account to use
*/}}
{{- define "dbbat.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "dbbat.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Build the PostgreSQL DSN from components if not provided
*/}}
{{- define "dbbat.dsn" -}}
{{- if .Values.postgresql.external.dsn }}
{{- .Values.postgresql.external.dsn }}
{{- else }}
{{- $host := required "postgresql.external.host is required when dsn is not set" .Values.postgresql.external.host }}
{{- $port := .Values.postgresql.external.port | default 5432 }}
{{- $database := .Values.postgresql.external.database | default "dbbat" }}
{{- $username := .Values.postgresql.external.username | default "dbbat" }}
{{- $sslMode := .Values.postgresql.external.sslMode | default "require" }}
{{- printf "postgres://%s@%s:%d/%s?sslmode=%s" $username $host (int $port) $database $sslMode }}
{{- end }}
{{- end }}

{{/*
Get the secret name for database credentials
*/}}
{{- define "dbbat.secretName" -}}
{{- if .Values.secrets.existingSecret }}
{{- .Values.secrets.existingSecret }}
{{- else }}
{{- include "dbbat.fullname" . }}
{{- end }}
{{- end }}
