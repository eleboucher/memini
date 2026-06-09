{{- if .Values.serviceAccount.create }}
apiVersion: v1
kind: ServiceAccount
metadata:
  name: {{ include "memini.serviceAccountName" . }}
  labels:
    {{- include "memini.labels" . | nindent 4 }}
{{- end }}
