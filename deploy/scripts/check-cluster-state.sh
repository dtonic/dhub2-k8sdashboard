#!/usr/bin/env sh
set -eu
ROOT=$(CDPATH= cd -- "$(dirname "$0")/../.." && pwd)
TMP_BASE=$(realpath "${TMPDIR:-/tmp}"); TMP=$(mktemp -d "$TMP_BASE/issue25-helm.XXXXXX")
cleanup() { case "$(realpath "$TMP")" in "$TMP_BASE"/issue25-helm.*) rm -rf -- "$TMP";; *) return 1;; esac; }; trap cleanup EXIT HUP INT TERM
HELM='alpine/helm:3.17.3@sha256:d899e6316789fec04ee95300a18e454b7942539cbb3d89bde3e0655d6ca2e895'
KUBECONFORM='ghcr.io/yannh/kubeconform:v0.6.7@sha256:0925177fb05b44ce18574076141b5c3d83235e1904d3f952182ac99ddc45762c'
MAIN=/work/deploy/helm/observability-dashboard; AGENT=/work/deploy/helm/cluster-state-agent
helm() { docker run --rm -v "$ROOT:/work:ro" "$HELM" "$@"; }
helm lint "$MAIN"; helm lint "$AGENT" --values "$AGENT/values-example.yaml"
for env in dev stage prod; do
  helm template dashboard "$MAIN" --values "$MAIN/values-$env.yaml" > "$TMP/$env.yaml"
  helm template dashboard "$MAIN" --values "$MAIN/values-$env.yaml" --set clusterState.mode=direct > "$TMP/$env-direct.yaml"
  cmp "$TMP/$env.yaml" "$TMP/$env-direct.yaml"
done
helm template dashboard "$MAIN" --values "$MAIN/values-dev.yaml" --values "$MAIN/values-central-example.yaml" > "$TMP/central.yaml"
helm template agent "$AGENT" --values "$AGENT/values-example.yaml" > "$TMP/agent.yaml"
helm template agent "$AGENT" --values "$AGENT/values-example.yaml" --set 'registry.cidrs[0]=128.0.0.0/1' --set 'kubernetesApiCidrs[0]=192.0.2.1/32' >/dev/null
helm template agent "$AGENT" --values "$AGENT/values-example.yaml" --set limits.maxChunkResources=1000 >/dev/null
docker run --rm -i "$KUBECONFORM" -strict -summary -kubernetes-version 1.31.0 < "$TMP/central.yaml"
docker run --rm -i "$KUBECONFORM" -strict -summary -kubernetes-version 1.31.0 < "$TMP/agent.yaml"
python3 - "$TMP/central.yaml" "$TMP/agent.yaml" <<'PY'
import sys,yaml
c=[x for x in yaml.safe_load_all(open(sys.argv[1])) if x]; a=[x for x in yaml.safe_load_all(open(sys.argv[2])) if x]
api=next(x for x in c if x.get('kind')=='Deployment' and x['metadata']['name'].endswith('-api')); reg=next(x for x in c if x.get('kind')=='Deployment' and x['metadata']['name'].endswith('-cluster-state-registry'))
assert api['spec']['template']['spec']['automountServiceAccountToken'] is False
assert reg['spec']['replicas']==1 and reg['spec']['template']['spec']['automountServiceAccountToken'] is False
services=[x for x in c if x.get('kind')=='Service' and 'cluster-state-registry' in x['metadata']['name']]
assert len(services)==2 and {tuple(p['name'] for p in x['spec']['ports']) for x in services}=={('query',),('agent',)}
assert not any(x['kind'] in ('ClusterRole','ClusterRoleBinding') and x['metadata']['name'].endswith('-api-read') for x in c)
api_np=next(x for x in c if x.get('kind')=='NetworkPolicy' and x['metadata']['name'].endswith('-api'))
assert not any(e.get('to')==[{'ipBlock':{'cidr':'10.96.0.1/32'}}] for e in api_np['spec']['egress'])
reg_np=next(x for x in c if x.get('kind')=='NetworkPolicy' and x['metadata']['name'].endswith('-cluster-state-registry'))
assert reg_np['spec']['ingress'][0]['ports']==[{'protocol':'TCP','port':9444}]
assert reg_np['spec']['ingress'][1]=={'from':[{'ipBlock':{'cidr':'192.0.2.0/24'}}],'ports':[{'protocol':'TCP','port':9443}]}
agent=next(x for x in a if x.get('kind')=='Deployment'); assert agent['spec']['replicas']==1 and agent['spec']['strategy']=={'type':'Recreate'} and agent['spec']['template']['spec']['containers'][0]['command']==['/cluster-state-agent']
np=next(x for x in a if x.get('kind')=='NetworkPolicy'); assert np['spec']['ingress']==[]
role=next(x for x in a if x.get('kind')=='ClusterRole'); assert all(r['verbs']==['get','list','watch'] for r in role['rules'])
for pod in (api,reg,agent):
 s=pod['spec']['template']['spec']; ct=s['containers'][0]
 assert s['securityContext']['runAsNonRoot'] and s['securityContext']['seccompProfile']['type']=='RuntimeDefault'
 assert ct['securityContext']['readOnlyRootFilesystem'] and ct['securityContext']['capabilities']['drop']==['ALL'] and ct.get('resources')
PY
expect_main_fail() { if helm template dashboard "$MAIN" --values "$MAIN/values-dev.yaml" --values "$MAIN/values-central-example.yaml" "$@" >/dev/null 2>&1; then echo "central mutation passed: $*" >&2; exit 1; fi; }
expect_agent_fail() { if helm template agent "$AGENT" --values "$AGENT/values-example.yaml" "$@" >/dev/null 2>&1; then echo "agent mutation passed: $*" >&2; exit 1; fi; }
expect_main_fail --set clusterState.tls.apiExistingSecret=
expect_main_fail --set clusterState.tls.registryExistingSecret=cluster-state-api-mtls
expect_main_fail --set clusterState.tls.certKey=
expect_main_fail --set clusterState.registryEndpoint=external.example:9444
expect_main_fail --set clusterState.registryServerName='bad/name'
expect_main_fail --set clusterState.registryServerName=a..b
expect_main_fail --set clusterState.registryServerName=a-.b
expect_main_fail --set clusterState.registryServerName=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.test
expect_main_fail --set clusterState.registryServerName=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
expect_main_fail --set clusterState.registry.image.digest=latest
expect_main_fail --set clusterState.registry.replicas=2
expect_main_fail --set-json 'clusterState.registry.agentService.sourceCidrs=[]'
expect_main_fail --set 'clusterState.registry.agentService.sourceCidrs[0]=0.0.0.0/0'
expect_main_fail --set 'clusterState.registry.agentService.sourceCidrs[0]=192.0.2.1/0'
expect_main_fail --set 'clusterState.registry.agentService.sourceCidrs[0]=garbage/x'
expect_main_fail --set 'clusterState.registry.agentService.sourceCidrs[0]=256.0.0.1/24'
expect_main_fail --set 'clusterState.registry.agentService.sourceCidrs[0]=192.0.2.1/33'
expect_main_fail --set-json 'clusterState.clusters=["UPPER"]'
expect_main_fail --set clusterState.limits.maxMessageBytes=4194305
expect_main_fail --set clusterState.limits.maxResources=1 --set clusterState.limits.maxChunkResources=1000
expect_main_fail --set clusterState.limits.maxMessageBytes=4194304 --set clusterState.limits.maxStateBytes=4194303 --set clusterState.limits.maxTotalStateBytes=4194303
expect_main_fail --set clusterState.limits.ingressByteRate=0
expect_main_fail --set clusterState.limits.ingressFrameRate=100000.1
expect_main_fail --set clusterState.limits.ingressByteRate=1073741825
expect_main_fail --set clusterState.limits.ingressFrameBurst=100001
expect_main_fail --set clusterState.limits.ingressByteBurst=1073741825
expect_main_fail --set clusterState.limits.maxMessageBytes=4194304 --set clusterState.limits.ingressByteBurst=4194303
expect_main_fail --set clusterState.limits.ingressFrameRate=.nan
expect_main_fail --set clusterState.limits.staleTTL=24h1s
expect_main_fail --set clusterState.limits.heartbeatTimeout=1h1s
expect_main_fail --set clusterState.limits.staleTTL=30s --set clusterState.limits.heartbeatTimeout=31s
helm template dashboard "$MAIN" --values "$MAIN/values-dev.yaml" --values "$MAIN/values-central-example.yaml" --set clusterState.limits.ingressFrameRate=100000 --set clusterState.limits.ingressByteRate=1073741824 --set clusterState.limits.ingressFrameBurst=100000 --set clusterState.limits.ingressByteBurst=1073741824 --set clusterState.limits.staleTTL=24h --set clusterState.limits.heartbeatTimeout=1h >/dev/null
helm template dashboard "$MAIN" --values "$MAIN/values-dev.yaml" --values "$MAIN/values-central-example.yaml" --set clusterState.limits.maxMessageBytes=4194304 --set clusterState.limits.ingressByteBurst=4194304 >/dev/null
helm template dashboard "$MAIN" --values "$MAIN/values-dev.yaml" --values "$MAIN/values-central-example.yaml" --set clusterState.limits.maxResources=1000 --set clusterState.limits.maxChunkResources=1000 --set clusterState.limits.maxMessageBytes=4194304 --set clusterState.limits.maxStateBytes=4194304 --set clusterState.limits.maxTotalStateBytes=4194304 --set clusterState.registry.resources.limits.memory=8Mi >/dev/null
expect_main_fail --set clusterState.registry.resources.limits.memory=512Mi
expect_main_fail --set api.config.AUTH_MODE=mock
expect_agent_fail --set tls.existingSecret=
expect_agent_fail --set image.digest=latest
expect_agent_fail --set clusterID=UPPER --set telemetryClusterName=UPPER
expect_agent_fail --set telemetryClusterName=cluster-b
expect_agent_fail --set-json 'registry.cidrs=[]'
expect_agent_fail --set 'registry.cidrs[0]=0.0.0.0/0'
expect_agent_fail --set 'registry.cidrs[0]=192.0.2.1/0'
expect_agent_fail --set 'registry.cidrs[0]=garbage/x'
expect_agent_fail --set 'registry.cidrs[0]=256.0.0.1/24'
expect_agent_fail --set 'registry.cidrs[0]=192.0.2.1/33'
expect_agent_fail --set-json 'kubernetesApiCidrs=[]'
expect_agent_fail --set 'kubernetesApiCidrs[0]=300.0.0.1/32'
expect_agent_fail --set registry.endpoint=:9443
expect_agent_fail --set registry.endpoint=registry.example:65536
expect_agent_fail --set registry.endpoint=https://registry.example:9443
expect_agent_fail --set registry.endpoint=registry.example/path:9443
expect_agent_fail --set registry.endpoint=user@registry.example:9443
expect_agent_fail --set registry.endpoint=a..b:9443
expect_agent_fail --set registry.endpoint=a.-b:9443
expect_agent_fail --set registry.serverName=bad/name
expect_agent_fail --set registry.serverName='*.example'
expect_agent_fail --set registry.serverName=a..b
expect_agent_fail --set registry.serverName=a-.b
expect_agent_fail --set registry.serverName=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.test
expect_agent_fail --set registry.serverName=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
expect_agent_fail --set registry.port=0
expect_agent_fail --set registry.port=9444
expect_agent_fail --set limits.maxChunkResources=0
expect_agent_fail --set limits.maxChunkResources=1001
expect_agent_fail --set limits.maxChunkResources=1000 --set limits.maxResources=999
expect_agent_fail --set tls.privateKeyKey=tls.crt
