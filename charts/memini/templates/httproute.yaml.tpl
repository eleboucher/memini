{{- if .Values.httpRoute.enabled }}
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: {{ include "memini.fullname" . }}
  labels:
    {{- include "memini.labels" . | nindent 4 }}
  {{- with .Values.httpRoute.annotations }}
  annotations:
    {{- toYaml . | nindent 4 }}
  {{- end }}
spec:
  parentRefs:
    {{- range .Values.httpRoute.parentRefs }}
    - name: {{ required "httpRoute.parentRefs[].name is required" .name }}
      {{- with .namespace }}
      namespace: {{ . }}
      {{- end }}
      {{- with .sectionName }}
      sectionName: {{ . }}
      {{- end }}
      {{- with .port }}
      port: {{ . }}
      {{- end }}
    {{- end }}
  {{- with .Values.httpRoute.hostnames }}
  hostnames:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  rules:
    - backendRefs:
        - name: {{ include "memini.fullname" . }}
          port: {{ .Values.service.port }}
{{- end }}
