{{- if .Values.auth.apiKey }}
apiVersion: v1
kind: Secret
metadata:
  name: {{ include "memini.fullname" . }}-auth
  labels:
    {{- include "memini.labels" . | nindent 4 }}
type: Opaque
stringData:
  api-key: {{ .Values.auth.apiKey | quote }}
{{- end }}
