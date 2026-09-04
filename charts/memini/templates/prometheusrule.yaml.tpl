{{- /*
Render the bundled alerting rules as a single PrometheusRule for the
prometheus-operator (a custom chart key, not part of the common library).

The rules cover memini's own degradation counters rather than generic
liveness: recall falling back to unranked order when the reranker or
embedder is unavailable, and a failing embed backend. Both are quiet
failures — memini keeps serving, so they surface as worse results rather
than as errors, and nothing else notices.
*/}}
{{- if .Values.prometheusRule.enabled }}
{{- $pr := .Values.prometheusRule }}
---
apiVersion: monitoring.coreos.com/v1
kind: PrometheusRule
metadata:
  name: {{ .Release.Name }}-memini
  labels:
    app.kubernetes.io/name: {{ .Chart.Name }}
    app.kubernetes.io/instance: {{ .Release.Name }}
    app.kubernetes.io/managed-by: {{ .Release.Service }}
    app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
    helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
    {{- with $pr.labels }}
    {{- toYaml . | nindent 4 }}
    {{- end }}
spec:
  groups:
    - name: memini.rules
      rules:
        {{- with $pr.rules.recallDegraded }}
        {{- if .enabled }}
        - alert: MeminiRecallDegraded
          expr: |-
            sum(increase(memini_recall_degraded_total[{{ .window }}])) > 0
          annotations:
            summary: >-
              Memini recall degraded — the reranker or embedder was unavailable and
              results fell back to unranked order
          labels:
            severity: {{ .severity | quote }}
        {{- end }}
        {{- end }}
        {{- with $pr.rules.embedErrors }}
        {{- if .enabled }}
        - alert: MeminiEmbedErrors
          expr: |-
            sum by (backend) (increase(memini_embed_errors_total[{{ .window }}])) > 0
          for: {{ .for }}
          annotations:
            summary: >-
              Memini embed backend {{ "{{ $labels.backend }}" }} is returning errors
          labels:
            severity: {{ .severity | quote }}
        {{- end }}
        {{- end }}
{{- end }}
