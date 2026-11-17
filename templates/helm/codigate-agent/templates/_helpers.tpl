{{- define "codigate-agent.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "codigate-agent.fullname" -}}
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

{{- define "codigate-agent.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "codigate-agent.selectorLabels" -}}
app.kubernetes.io/name: {{ include "codigate-agent.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "codigate-agent.labels" -}}
helm.sh/chart: {{ include "codigate-agent.chart" . }}
{{ include "codigate-agent.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "codigate-agent.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "codigate-agent.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{- define "codigate-agent.image" -}}
{{- if .Values.image.digest }}
{{- printf "%s@%s" .Values.image.repository .Values.image.digest }}
{{- else }}
{{- printf "%s:%s" .Values.image.repository (default (printf "v%s" .Chart.AppVersion) .Values.image.tag) }}
{{- end }}
{{- end }}

{{- define "codigate-agent.secretName" -}}
{{- default (include "codigate-agent.fullname" .) .Values.existingSecret }}
{{- end }}

{{- define "codigate-agent.configMapName" -}}
{{- default (include "codigate-agent.fullname" .) .Values.existingConfigMap }}
{{- end }}

{{- define "codigate-agent.configKey" -}}
{{- if .Values.existingConfigMap }}{{ .Values.existingConfigMapKey }}{{ else }}codigate.yml{{ end }}
{{- end }}

{{/* Checks that fail early, with a clear message, instead of a crashing pod. */}}
{{- define "codigate-agent.validate" -}}
{{- if and (not .Values.existingSecret) (not .Values.token) }}
{{- fail "set existingSecret (recommended) or token: the agent needs its token (cga_...)" }}
{{- end }}
{{- if not .Values.existingConfigMap }}
{{- $ingress := .Values.config.ingress | default list }}
{{- if not $ingress }}
{{- fail "config.ingress needs at least one rule; the last one without a hostname, e.g. service: http_status:404" }}
{{- end }}
{{- if (last $ingress).hostname }}
{{- fail "the last rule in config.ingress must have no hostname (a catch-all, e.g. service: http_status:404)" }}
{{- end }}
{{- if or (hasKey .Values.config "token") (hasKey .Values.config "token_file") }}
{{- fail "do not put the token in config; use existingSecret or token" }}
{{- end }}
{{- if not .Values.edge }}
{{- fail "edge is required, e.g. https://connect.codigate.nl" }}
{{- end }}
{{- if hasKey .Values.config "edge" }}
{{- fail "set edge at the top level of the values, not in config" }}
{{- end }}
{{- end }}
{{- end }}
