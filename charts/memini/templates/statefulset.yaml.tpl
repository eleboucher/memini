{{- if eq .Values.backend "sqlite" }}
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: {{ include "memini.fullname" . }}
  labels:
    {{- include "memini.labels" . | nindent 4 }}
spec:
  # sqlite is embedded and single-writer: exactly one replica.
  replicas: 1
  serviceName: {{ include "memini.fullname" . }}
  selector:
    matchLabels:
      {{- include "memini.selectorLabels" . | nindent 6 }}
  template:
    metadata:
      labels:
        {{- include "memini.selectorLabels" . | nindent 8 }}
      {{- with .Values.podAnnotations }}
      annotations:
        {{- toYaml . | nindent 8 }}
      {{- end }}
    spec:
      serviceAccountName: {{ include "memini.serviceAccountName" . }}
      {{- with .Values.imagePullSecrets }}
      imagePullSecrets:
        {{- toYaml . | nindent 8 }}
      {{- end }}
      securityContext:
        {{- toYaml .Values.podSecurityContext | nindent 8 }}
      containers:
        {{- include "memini.container" . | nindent 8 }}
      {{- if not .Values.sqlite.persistence.enabled }}
      volumes:
        - name: data
          emptyDir: {}
      {{- end }}
      {{- with .Values.nodeSelector }}
      nodeSelector:
        {{- toYaml . | nindent 8 }}
      {{- end }}
      {{- with .Values.tolerations }}
      tolerations:
        {{- toYaml . | nindent 8 }}
      {{- end }}
      {{- with .Values.affinity }}
      affinity:
        {{- toYaml . | nindent 8 }}
      {{- end }}
  {{- if .Values.sqlite.persistence.enabled }}
  volumeClaimTemplates:
    - metadata:
        name: data
      spec:
        accessModes:
          {{- toYaml .Values.sqlite.persistence.accessModes | nindent 10 }}
        {{- if .Values.sqlite.persistence.storageClass }}
        storageClassName: {{ .Values.sqlite.persistence.storageClass }}
        {{- end }}
        resources:
          requests:
            storage: {{ .Values.sqlite.persistence.size }}
  {{- end }}
{{- end }}
