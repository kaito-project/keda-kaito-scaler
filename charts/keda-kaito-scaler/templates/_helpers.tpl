{{/*
Expand the name of the chart.
*/}}
{{- define "keda-kaito-scaler.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "keda-kaito-scaler.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "keda-kaito-scaler.labels" -}}
helm.sh/chart: {{ include "keda-kaito-scaler.chart" . }}
{{ include "keda-kaito-scaler.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "keda-kaito-scaler.selectorLabels" -}}
app.kubernetes.io/name: {{ include "keda-kaito-scaler.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}