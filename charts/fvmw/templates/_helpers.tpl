{{/*
Resource name, based on the release name.
*/}}
{{- define "fvmw.fullname" -}}
{{- .Release.Name -}}
{{- end -}}

{{/*
Common labels.
*/}}
{{- define "fvmw.labels" -}}
app: fvmw
app.kubernetes.io/name: fvmw
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{/*
Selector labels (must stay stable across upgrades).
*/}}
{{- define "fvmw.selectorLabels" -}}
app: fvmw
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/*
Public hostname fvmw is reached at.
*/}}
{{- define "fvmw.host" -}}
{{- if .Values.host -}}
{{ .Values.host }}
{{- else if .Values.clusterDomain -}}
vcenter.{{ .Values.clusterDomain }}
{{- else -}}
{{ fail "Set .Values.host (e.g. vcenter.apps.cluster.example.com) or .Values.clusterDomain" }}
{{- end -}}
{{- end -}}

{{/*
Name of the Secret holding the fvmw login password.
*/}}
{{- define "fvmw.secretName" -}}
{{- if .Values.fvmw.existingSecret -}}
{{ .Values.fvmw.existingSecret }}
{{- else -}}
{{ include "fvmw.fullname" . }}-credentials
{{- end -}}
{{- end -}}

{{/*
Name of the shared disks PVC.
*/}}
{{- define "fvmw.pvcName" -}}
{{- if .Values.disks.existingClaim -}}
{{ .Values.disks.existingClaim }}
{{- else if .Values.disks.pvcName -}}
{{ .Values.disks.pvcName }}
{{- else -}}
{{ include "fvmw.fullname" . }}-disks
{{- end -}}
{{- end -}}
