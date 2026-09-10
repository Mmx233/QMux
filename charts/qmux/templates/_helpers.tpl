{{- define "qmux.name" -}}
{{- .Chart.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "qmux.fullname" -}}
{{- if contains .Chart.Name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name .Chart.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "qmux.headlessServiceName" -}}
{{- printf "%s-headless" (include "qmux.fullname" . | trunc 54 | trimSuffix "-") -}}
{{- end -}}

{{- define "qmux.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "qmux.selectorLabels" -}}
app.kubernetes.io/name: {{ include "qmux.name" . }}
app.kubernetes.io/instance: {{ .Release.Name | quote }}
app.kubernetes.io/component: {{ .Values.mode }}
{{- end -}}

{{- define "qmux.labels" -}}
helm.sh/chart: {{ include "qmux.chart" . }}
{{ include "qmux.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "qmux.workloadKind" -}}
{{- if .Values.workload.kind -}}
{{- .Values.workload.kind -}}
{{- else if eq .Values.mode "server" -}}
StatefulSet
{{- else -}}
Deployment
{{- end -}}
{{- end -}}

{{- define "qmux.replicaCount" -}}
{{- if ne .Values.workload.replicaCount nil -}}
{{- .Values.workload.replicaCount -}}
{{- else if eq .Values.mode "server" -}}
1
{{- else -}}
2
{{- end -}}
{{- end -}}

{{- define "qmux.adminPort" -}}
{{- if ne .Values.admin.port nil -}}
{{- .Values.admin.port -}}
{{- else if eq .Values.mode "server" -}}
9090
{{- else -}}
9091
{{- end -}}
{{- end -}}

{{- define "qmux.serviceAccountName" -}}
{{- if .Values.serviceAccount.name -}}
{{- .Values.serviceAccount.name -}}
{{- else if .Values.serviceAccount.create -}}
{{- include "qmux.fullname" . -}}
{{- else -}}
default
{{- end -}}
{{- end -}}

{{- define "qmux.configSource" -}}
{{- if ne (trim .Values.config.existing.name) "" -}}
existing
{{- else if ne (trim .Values.config.inlineYaml) "" -}}
inline
{{- else -}}
direct
{{- end -}}
{{- end -}}

{{- define "qmux.inlineEnvelope" -}}
{{- $inline := trim .Values.config.inlineYaml -}}
{{- $inline = regexReplaceAll `(?s)^([ \t]*(#[^\r\n]*)?\r?\n)*---([ \t]+|\r?\n|$)` $inline "" -}}
{{- $inline = regexReplaceAll `(?s)(^|\r?\n)\.\.\.[ \t]*(#[^\r\n]*)?(\r?\n[ \t]*(#[^\r\n]*)?)*$` $inline "" -}}
{{- if or (regexMatch `(?m)^---([ \t]|$)` $inline) (regexMatch `(?m)^\.\.\.[ \t]*(#.*)?$` $inline) -}}{{- fail "config.inlineYaml must be a parseable YAML object" -}}{{- end -}}
{{- printf "value:%s" (nindent 2 $inline) -}}
{{- end -}}

{{- define "qmux.generatedConfig" -}}
{{- $cfg := dict -}}
{{- $source := include "qmux.configSource" . -}}
{{- if eq $source "direct" -}}
  {{- $cfg = deepCopy (index .Values.config .Values.mode) -}}
{{- else if eq $source "inline" -}}
  {{- $wrapped := fromYaml (include "qmux.inlineEnvelope" .) -}}
  {{- $parsed := fromYaml .Values.config.inlineYaml -}}
  {{- if and (kindIs "map" $wrapped) (not (hasKey $wrapped "Error")) (hasKey $wrapped "value") (kindIs "map" (get $wrapped "value")) (kindIs "map" $parsed) (not (hasKey $parsed "Error")) -}}
    {{- $cfg = deepCopy $parsed -}}
  {{- end -}}
{{- end -}}

{{- $auth := dict -}}
{{- $authIsMap := false -}}
{{- if not (hasKey $cfg "auth") -}}
  {{- $_ := set $cfg "auth" $auth -}}
  {{- $authIsMap = true -}}
{{- else if kindIs "map" (get $cfg "auth") -}}
  {{- $auth = get $cfg "auth" -}}
  {{- $authIsMap = true -}}
{{- end -}}
{{- $effectiveMTLS := false -}}
{{- if $authIsMap -}}
  {{- if not (hasKey $auth "method") -}}
    {{- $effectiveMTLS = true -}}
  {{- else if kindIs "string" (get $auth "method") -}}
    {{- $method := get $auth "method" -}}
    {{- if or (eq $method "") (eq $method "mtls") -}}
      {{- $effectiveMTLS = true -}}
    {{- end -}}
  {{- end -}}
{{- end -}}

{{- $tls := dict -}}
{{- $tlsIsMap := false -}}
{{- if not (hasKey $cfg "tls") -}}
  {{- $_ := set $cfg "tls" $tls -}}
  {{- $tlsIsMap = true -}}
{{- else if kindIs "map" (get $cfg "tls") -}}
  {{- $tls = get $cfg "tls" -}}
  {{- $tlsIsMap = true -}}
{{- end -}}

{{- if and .Values.admin.enabled (not (hasKey $cfg "admin_address")) -}}
  {{- $_ := set $cfg "admin_address" (printf "0.0.0.0:%s" (include "qmux.adminPort" .)) -}}
{{- end -}}
{{- if eq .Values.mode "server" -}}
  {{- if $tlsIsMap -}}
    {{- if not (hasKey $tls "server_cert_file") -}}{{- $_ := set $tls "server_cert_file" "/etc/qmux/tls/tls.crt" -}}{{- end -}}
    {{- if not (hasKey $tls "server_key_file") -}}{{- $_ := set $tls "server_key_file" "/etc/qmux/tls/tls.key" -}}{{- end -}}
  {{- end -}}
  {{- if and $effectiveMTLS $authIsMap (not (hasKey $auth "ca_cert_file")) -}}
    {{- $_ := set $auth "ca_cert_file" "/etc/qmux/tls/ca.crt" -}}
  {{- end -}}
{{- else -}}
  {{- if $tlsIsMap -}}
    {{- if not (hasKey $tls "ca_cert_file") -}}{{- $_ := set $tls "ca_cert_file" "/etc/qmux/tls/ca.crt" -}}{{- end -}}
    {{- if $effectiveMTLS -}}
      {{- if not (hasKey $tls "client_cert_file") -}}{{- $_ := set $tls "client_cert_file" "/etc/qmux/tls/tls.crt" -}}{{- end -}}
      {{- if not (hasKey $tls "client_key_file") -}}{{- $_ := set $tls "client_key_file" "/etc/qmux/tls/tls.key" -}}{{- end -}}
    {{- end -}}
  {{- end -}}
{{- end -}}
{{ toYaml $cfg }}
{{- end -}}

{{- define "qmux.configHasToken" -}}
{{- $cfg := include "qmux.generatedConfig" . | fromYaml -}}
{{- if and (kindIs "map" $cfg) (hasKey $cfg "auth") (kindIs "map" (get $cfg "auth")) (hasKey (get $cfg "auth") "token") -}}true{{- else -}}false{{- end -}}
{{- end -}}

{{- define "qmux.configResourceKind" -}}
{{- if eq (include "qmux.configSource" .) "existing" -}}
{{- .Values.config.existing.kind -}}
{{- else if eq (include "qmux.configHasToken" .) "true" -}}
Secret
{{- else -}}
ConfigMap
{{- end -}}
{{- end -}}

{{- define "qmux.configResourceName" -}}
{{- if eq (include "qmux.configSource" .) "existing" -}}
{{- .Values.config.existing.name -}}
{{- else -}}
{{- printf "%s-config" (include "qmux.fullname" .) -}}
{{- end -}}
{{- end -}}

{{- define "qmux.tlsIdentitySecretName" -}}
{{- if eq .Values.tls.mode "certManager" -}}{{- .Values.tls.certManager.identitySecretName -}}{{- else -}}{{- .Values.tls.existingSecret.name -}}{{- end -}}
{{- end -}}

{{- define "qmux.tlsTrustSecretName" -}}
{{- if eq .Values.tls.mode "certManager" -}}{{- .Values.tls.certManager.trustSecretName -}}{{- else -}}{{- .Values.tls.existingSecret.name -}}{{- end -}}
{{- end -}}

{{- define "qmux.validate" -}}
{{- $source := include "qmux.configSource" . -}}
{{- if eq $source "direct" -}}
  {{- $cfg := index .Values.config .Values.mode -}}
  {{- if eq .Values.mode "server" -}}
    {{- if or (not (hasKey $cfg "listeners")) (not (kindIs "slice" (get $cfg "listeners"))) (eq (len (get $cfg "listeners")) 0) -}}{{- fail "config.server.listeners must contain at least one listener" -}}{{- end -}}
    {{- range $i, $listener := get $cfg "listeners" -}}
      {{- if not (kindIs "map" $listener) -}}{{- fail (printf "config.server.listeners[%d] must be an object" $i) -}}{{- end -}}
      {{- range $field := list "quic_addr" "traffic_addr" "protocol" -}}
        {{- if or (not (hasKey $listener $field)) (empty (get $listener $field)) -}}{{- fail (printf "config.server.listeners[%d].%s is required" $i $field) -}}{{- end -}}
      {{- end -}}
      {{- if not (has (get $listener "protocol") (list "tcp" "udp" "both")) -}}{{- fail (printf "config.server.listeners[%d].protocol must be tcp, udp, or both" $i) -}}{{- end -}}
    {{- end -}}
  {{- else -}}
    {{- $errors := list -}}
    {{- $servers := list -}}
    {{- if and (hasKey $cfg "server") (kindIs "map" (get $cfg "server")) (hasKey (get $cfg "server") "servers") (kindIs "slice" (get (get $cfg "server") "servers")) (gt (len (get (get $cfg "server") "servers")) 0) -}}
      {{- $servers = get (get $cfg "server") "servers" -}}
    {{- else -}}
      {{- $errors = append $errors "config.client.server.servers must contain at least one server" -}}
    {{- end -}}
    {{- $local := dict -}}
    {{- if and (hasKey $cfg "local") (kindIs "map" (get $cfg "local")) -}}
      {{- $local = get $cfg "local" -}}
      {{- if or (not (hasKey $local "host")) (empty (get $local "host")) -}}{{- $errors = append $errors "config.client.local.host is required" -}}{{- end -}}
      {{- if or (not (hasKey $local "port")) (le (int (get $local "port")) 0) (gt (int (get $local "port")) 65535) -}}{{- $errors = append $errors "config.client.local.port must be between 1 and 65535" -}}{{- end -}}
    {{- else -}}
      {{- $errors = append $errors "config.client.local.host is required" -}}
      {{- $errors = append $errors "config.client.local.port must be between 1 and 65535" -}}
    {{- end -}}
    {{- if $errors -}}{{- fail (join "; " $errors) -}}{{- end -}}
    {{- range $i, $server := $servers -}}
      {{- if not (kindIs "map" $server) -}}{{- fail (printf "config.client.server.servers[%d] must be an object" $i) -}}{{- end -}}
      {{- range $field := list "address" "server_name" -}}
        {{- if or (not (hasKey $server $field)) (empty (get $server $field)) -}}{{- fail (printf "config.client.server.servers[%d].%s is required" $i $field) -}}{{- end -}}
      {{- end -}}
    {{- end -}}
  {{- end -}}
  {{- $method := "mtls" -}}
  {{- if and (hasKey $cfg "auth") (kindIs "map" (get $cfg "auth")) (hasKey (get $cfg "auth") "method") -}}{{- $method = get (get $cfg "auth") "method" -}}{{- end -}}
  {{- if not (has $method (list "mtls" "token")) -}}{{- fail (printf "config.%s.auth.method must be mtls or token" .Values.mode) -}}{{- end -}}
  {{- if eq $method "token" -}}
    {{- if or (not (hasKey $cfg "auth")) (not (kindIs "map" (get $cfg "auth"))) (empty (get (get $cfg "auth") "token")) -}}{{- fail (printf "config.%s.auth.token is required for token authentication" .Values.mode) -}}{{- end -}}
  {{- end -}}
{{- else if eq $source "inline" -}}
  {{- $parsed := fromYaml .Values.config.inlineYaml -}}
  {{- $wrapped := fromYaml (include "qmux.inlineEnvelope" .) -}}
  {{- if or (hasKey $parsed "Error") (not (kindIs "map" $wrapped)) (hasKey $wrapped "Error") (not (hasKey $wrapped "value")) (not (kindIs "map" (get $wrapped "value"))) -}}{{- fail "config.inlineYaml must be a parseable YAML object" -}}{{- end -}}
{{- end -}}

{{- if eq .Values.tls.mode "existingSecret" -}}
  {{- if empty .Values.tls.existingSecret.name -}}{{- fail "tls.existingSecret.name is required" -}}{{- end -}}
{{- else -}}
  {{- if not (.Capabilities.APIVersions.Has "cert-manager.io/v1/Certificate") -}}{{- fail "tls.mode=certManager requires cert-manager.io/v1/Certificate" -}}{{- end -}}
  {{- if empty .Values.tls.certManager.issuerRef.name -}}{{- fail "tls.certManager.issuerRef.name is required" -}}{{- end -}}
  {{- if empty .Values.tls.certManager.identitySecretName -}}{{- fail "tls.certManager.identitySecretName is required" -}}{{- end -}}
  {{- if empty .Values.tls.certManager.trustSecretName -}}{{- fail "tls.certManager.trustSecretName is required" -}}{{- end -}}
  {{- if eq .Values.tls.certManager.identitySecretName .Values.tls.certManager.trustSecretName -}}{{- fail "tls.certManager.identitySecretName and tls.certManager.trustSecretName must differ" -}}{{- end -}}
  {{- if and .Values.tls.certManager.includeHeadlessServiceDNS (not (and (eq .Values.mode "server") (eq (include "qmux.workloadKind" .) "StatefulSet"))) -}}{{- fail "tls.certManager.includeHeadlessServiceDNS is valid only for a server StatefulSet" -}}{{- end -}}
  {{- if and (eq .Values.mode "server") (eq (len .Values.tls.certManager.dnsNames) 0) (not .Values.tls.certManager.includeHeadlessServiceDNS) -}}{{- fail "tls.certManager.dnsNames is required when server headless Service DNS is disabled" -}}{{- end -}}
{{- end -}}

{{- if and (ne .Values.pdb.minAvailable nil) (ne .Values.pdb.maxUnavailable nil) -}}{{- fail "pdb.minAvailable and pdb.maxUnavailable are mutually exclusive" -}}{{- end -}}
{{- if and .Values.pdb.enabled (eq .Values.pdb.minAvailable nil) (eq .Values.pdb.maxUnavailable nil) -}}{{- fail "pdb.enabled=true requires pdb.minAvailable or pdb.maxUnavailable" -}}{{- end -}}
{{- if not .Values.admin.enabled -}}
  {{- if or .Values.probes.liveness.enabled .Values.probes.readiness.enabled (and .Values.monitoring.enabled (or .Values.monitoring.prometheusAnnotations.enabled .Values.monitoring.podMonitor.enabled)) -}}{{- fail "admin.enabled=false requires probes and metrics collection to be disabled" -}}{{- end -}}
{{- end -}}
{{- if and .Values.monitoring.enabled .Values.monitoring.podMonitor.enabled (not (.Capabilities.APIVersions.Has "monitoring.coreos.com/v1/PodMonitor")) -}}{{- fail "monitoring.podMonitor.enabled=true requires monitoring.coreos.com/v1/PodMonitor" -}}{{- end -}}
{{- end -}}
