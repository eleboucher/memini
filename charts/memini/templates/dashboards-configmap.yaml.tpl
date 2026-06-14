{{- /*
Render the bundled Grafana dashboard as a single ConfigMap with the
grafana_dashboard "1" label so the grafana-operator's auto-loader picks
it up via the Grafana CR's dashboardsConfigMaps selector.

The chart ships a single dashboard (dashboards/memini.json) and exposes it
under the data key memini.json. This template is self-contained (it reads
.Files, which the common library cannot do from values).
*/}}
{{- if .Values.grafanaDashboards.enabled }}
{{- $raw := .Files.Get "dashboards/memini.json" }}
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ .Release.Name }}-memini
  labels:
    app.kubernetes.io/name: {{ .Chart.Name }}
    app.kubernetes.io/instance: {{ .Release.Name }}
    app.kubernetes.io/managed-by: {{ .Release.Service }}
    app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
    helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
    grafana_dashboard: "1"
  annotations:
    grafana_dashboard_folder: {{ .Values.grafanaDashboards.folder | default "memini" | quote }}
    meta.helm.sh/release-name: {{ .Release.Name | quote }}
    meta.helm.sh/release-namespace: {{ .Release.Namespace | quote }}
data:
  memini.json: |-
{{ $raw | nindent 4 }}
{{- end }}
