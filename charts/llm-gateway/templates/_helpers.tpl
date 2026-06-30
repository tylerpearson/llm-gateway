{{/*
Expand the name of the chart.
*/}}
{{- define "llm-gateway.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
If release name contains chart name it will be used as a full name.
*/}}
{{- define "llm-gateway.fullname" -}}
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
Create chart label value, e.g. "llm-gateway-0.1.0".
*/}}
{{- define "llm-gateway.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels applied to every resource.
*/}}
{{- define "llm-gateway.labels" -}}
helm.sh/chart: {{ include "llm-gateway.chart" . }}
{{ include "llm-gateway.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels used by Services and Deployments.
*/}}
{{- define "llm-gateway.selectorLabels" -}}
app.kubernetes.io/name: {{ include "llm-gateway.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Create the name of the ServiceAccount to use.
*/}}
{{- define "llm-gateway.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "llm-gateway.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Name of the Secret that holds provider keys and DSNs.
When secretEnv.create is false, returns the existingSecret name.
*/}}
{{- define "llm-gateway.secretName" -}}
{{- if .Values.secretEnv.create }}
{{- include "llm-gateway.fullname" . }}
{{- else }}
{{- .Values.secretEnv.existingSecret }}
{{- end }}
{{- end }}

{{/*
Name of the ConfigMap that holds the gateway config.yaml.
*/}}
{{- define "llm-gateway.configMapName" -}}
{{- printf "%s-config" (include "llm-gateway.fullname" .) }}
{{- end }}
