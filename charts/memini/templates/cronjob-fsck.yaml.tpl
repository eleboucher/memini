{{- if .Values.fsck.cronjob.enabled }}
apiVersion: batch/v1
kind: CronJob
metadata:
  name: {{ include "memini.fullname" . }}-fsck
  labels:
    {{- include "memini.labels" . | nindent 4 }}
spec:
  schedule: {{ .Values.fsck.cronjob.schedule | quote }}
  concurrencyPolicy: Forbid
  jobTemplate:
    spec:
      template:
        spec:
          restartPolicy: Never
          containers:
            - name: fsck
              image: {{ .Values.fsck.cronjob.image }}
              {{- if include "memini.authEnabled" . }}
              env:
                - name: MEMINI_API_KEY
                  valueFrom:
                    secretKeyRef:
                      name: {{ include "memini.authSecretName" . }}
                      key: {{ include "memini.authSecretKey" . }}
              {{- end }}
              args:
                - -sf
                - -X
                - POST
                {{- if include "memini.authEnabled" . }}
                - -H
                - "Authorization: Bearer $(MEMINI_API_KEY)"
                {{- end }}
                - http://{{ include "memini.fullname" . }}:{{ .Values.service.port }}/v1/fsck
{{- end }}
