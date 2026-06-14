---
{{- include "bjw-s.common.loader.init" . }}

{{- /*
  The common library (v5.0.1) rejects an empty image tag with no digest. memini
  keeps tag "" literal in values.yaml (the release pipeline rewrites the
  image block), so default the tag to the chart appVersion at render time when
  neither tag nor digest is set.
*/}}
{{- $img := .Values.controllers.main.containers.main.image }}
{{- if and (not $img.tag) (not $img.digest) }}
{{- $_ := set $img "tag" .Chart.AppVersion }}
{{- end }}

{{- include "bjw-s.common.loader.generate" . }}
