{{- define "gitone.labels" -}}
app.kubernetes.io/name: gitone
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{- end }}

{{- define "gitone.selectorLabels" -}}
app.kubernetes.io/name: gitone
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "gitone.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default "gitone" .Values.serviceAccount.name -}}
{{- else -}}
{{- required "serviceAccount.name is required when serviceAccount.create is false" .Values.serviceAccount.name -}}
{{- end -}}
{{- end }}

{{- define "gitone.validateValues" -}}
{{- if gt (int .Values.pack.maxSmallPackCount) (int .Values.pack.maxPackCount) -}}
{{- fail "pack.maxSmallPackCount cannot exceed pack.maxPackCount" -}}
{{- end -}}
{{- if gt (int .Values.path.maxTopLevelLength) (int .Values.path.maxComponentLength) -}}
{{- fail "path.maxTopLevelLength cannot exceed path.maxComponentLength" -}}
{{- end -}}
{{- if and .Values.s3.endpoint .Values.s3.tls (hasPrefix "http://" .Values.s3.endpoint) -}}
{{- fail "s3.tls=true requires an https endpoint" -}}
{{- end -}}
{{- if and .Values.s3.endpoint (not .Values.s3.tls) (hasPrefix "https://" .Values.s3.endpoint) -}}
{{- fail "s3.tls=false requires an http endpoint" -}}
{{- end -}}
{{- if and (not .Values.s3.endpoint) (not .Values.s3.tls) -}}
{{- fail "s3.tls=false requires an explicit http endpoint" -}}
{{- end -}}
{{- end }}
