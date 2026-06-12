{{/* Chart name */}}
{{- define "memini.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* Fully-qualified app name */}}
{{- define "memini.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "memini.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "memini.labels" -}}
helm.sh/chart: {{ include "memini.chart" . }}
{{ include "memini.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "memini.selectorLabels" -}}
app.kubernetes.io/name: {{ include "memini.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "memini.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "memini.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{- define "memini.image" -}}
{{- if .Values.image.digest -}}
{{- printf "%s@%s" .Values.image.repository .Values.image.digest -}}
{{- else -}}
{{- printf "%s:%s" .Values.image.repository (default .Chart.AppVersion .Values.image.tag) -}}
{{- end -}}
{{- end -}}

{{/* Container env, derived from values + secret refs */}}
{{- define "memini.env" -}}
- name: MEMINI_HTTP_ADDR
  value: ":{{ .Values.service.port }}"
- name: MEMINI_BACKEND
  value: {{ .Values.backend | quote }}
- name: MEMINI_LOG_LEVEL
  value: {{ .Values.config.logLevel | quote }}
- name: MEMINI_LOG_FORMAT
  value: {{ .Values.config.logFormat | quote }}
- name: MEMINI_DEFAULT_NAMESPACE
  value: {{ .Values.config.defaultNamespace | quote }}
- name: MEMINI_NAMESPACE_HEADER
  value: {{ .Values.config.namespaceHeader | quote }}
- name: MEMINI_SWEEP_INTERVAL
  value: {{ .Values.config.sweepInterval | quote }}
- name: MEMINI_EMBED_BASE_URL
  value: {{ .Values.embeddings.baseURL | quote }}
- name: MEMINI_EMBED_MODEL
  value: {{ .Values.embeddings.model | quote }}
- name: MEMINI_EMBED_DIMS
  value: {{ .Values.embeddings.dims | quote }}
{{- if .Values.embeddings.apiKeySecret.name }}
- name: MEMINI_EMBED_API_KEY
  valueFrom:
    secretKeyRef:
      name: {{ .Values.embeddings.apiKeySecret.name }}
      key: {{ .Values.embeddings.apiKeySecret.key }}
{{- end }}
{{- if eq .Values.backend "sqlite" }}
- name: MEMINI_SQLITE_PATH
  value: /data/memini.db
{{- end }}
{{- if eq .Values.backend "postgres" }}
- name: MEMINI_POSTGRES_DSN
  valueFrom:
    secretKeyRef:
      name: {{ required "postgres.dsnSecret.name is required when backend=postgres" .Values.postgres.dsnSecret.name }}
      key: {{ .Values.postgres.dsnSecret.key }}
{{- end }}
{{- if .Values.llm.baseURL }}
- name: MEMINI_LLM_BASE_URL
  value: {{ .Values.llm.baseURL | quote }}
- name: MEMINI_LLM_MODEL
  value: {{ .Values.llm.model | quote }}
{{- if .Values.llm.apiKeySecret.name }}
- name: MEMINI_LLM_API_KEY
  valueFrom:
    secretKeyRef:
      name: {{ .Values.llm.apiKeySecret.name }}
      key: {{ .Values.llm.apiKeySecret.key }}
{{- end }}
{{- end }}
{{- if and .Values.rerank.mode (ne .Values.rerank.mode "off") }}
- name: MEMINI_RERANK
  value: {{ .Values.rerank.mode | quote }}
{{- with .Values.rerank.model }}
- name: MEMINI_RERANK_MODEL
  value: {{ . | quote }}
{{- end }}
- name: MEMINI_RERANK_TOP_N
  value: {{ .Values.rerank.topN | quote }}
{{- if ne (toString .Values.rerank.maxDocChars) "" }}
- name: MEMINI_RERANK_MAX_DOC_CHARS
  value: {{ .Values.rerank.maxDocChars | quote }}
{{- end }}
{{- if .Values.rerank.apiKeySecret.name }}
- name: MEMINI_RERANK_API_KEY
  valueFrom:
    secretKeyRef:
      name: {{ .Values.rerank.apiKeySecret.name }}
      key: {{ .Values.rerank.apiKeySecret.key }}
{{- end }}
{{- end }}
{{- if include "memini.authEnabled" . }}
- name: MEMINI_API_KEY
  valueFrom:
    secretKeyRef:
      name: {{ include "memini.authSecretName" . }}
      key: {{ include "memini.authSecretKey" . }}
{{- end }}
- name: MEMINI_UI_ENABLED
  value: {{ .Values.ui.enabled | quote }}
{{- with .Values.extraEnv }}
{{ toYaml . }}
{{- end }}
{{- end -}}

{{/* Shared container spec */}}
{{- define "memini.container" -}}
- name: memini
  image: {{ include "memini.image" . }}
  imagePullPolicy: {{ .Values.image.pullPolicy }}
  ports:
    - name: http
      containerPort: {{ .Values.service.port }}
  env:
    {{- include "memini.env" . | nindent 4 }}
  livenessProbe:
    httpGet:
      path: /healthz
      port: http
    initialDelaySeconds: 5
    periodSeconds: 15
  readinessProbe:
    httpGet:
      path: /readyz
      port: http
    initialDelaySeconds: 3
    periodSeconds: 10
  securityContext:
    {{- toYaml .Values.securityContext | nindent 4 }}
  resources:
    {{- toYaml .Values.resources | nindent 4 }}
  {{- if eq .Values.backend "sqlite" }}
  volumeMounts:
    - name: data
      mountPath: /data
  {{- end }}
{{- end -}}

{{/* Auth: whether a token is configured (created or referenced) */}}
{{- define "memini.authEnabled" -}}
{{- if or .Values.auth.apiKey .Values.auth.apiKeySecret.name -}}true{{- end -}}
{{- end -}}

{{/* Auth: secret name holding the API key (chart-created when auth.apiKey set) */}}
{{- define "memini.authSecretName" -}}
{{- if .Values.auth.apiKey -}}{{ include "memini.fullname" . }}-auth{{- else -}}{{ .Values.auth.apiKeySecret.name }}{{- end -}}
{{- end -}}

{{/* Auth: key within the secret */}}
{{- define "memini.authSecretKey" -}}
{{- if .Values.auth.apiKey -}}api-key{{- else -}}{{ .Values.auth.apiKeySecret.key }}{{- end -}}
{{- end -}}
