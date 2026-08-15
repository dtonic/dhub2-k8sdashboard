# 멀티 클러스터 상태 운영 런북

## 온보딩

1. canonical cluster ID를 collector `telemetry.clusterName`, Agent `clusterID`, 중앙 `clusterState.clusters`에 동일하게 설정한다.
2. Registry server, API client, Agent client leaf/key는 서로 다른 기존 Secret에 배치한다. 공용 trust root를 쓸 수 있지만 CA private key는 어떤 workload에도 mount하지 않는다. SPIFFE URI SAN 역할과 cluster ID가 정확히 일치해야 한다.
3. 중앙 chart를 `clusterState.mode=central`, OIDC, immutable digest, Greptime/Quickwit cluster mapping으로 렌더한다.
4. 각 원격 클러스터에 별도 `cluster-state-agent` chart를 렌더한다. Registry와 Kubernetes API의 정확한 IPv4 CIDR/port만 허용한다. 현재 chart는 IPv6 CIDR를 지원하지 않으며, 모든 `/0`은 명시적으로 거부하고 `/1`~`/32`만 허용한다.
5. `make deploy-check api-central-itest`를 통과시킨다. 저장소 검증은 cluster apply를 수행하지 않는다.

## 인증서 회전·폐기

- 새 CA/server/API/Agent 파일을 Secret에 원자 교체한다. 클라이언트는 재연결마다 파일을 다시 읽는다.
- CA 교체는 새 CA 신뢰, leaf 교체, 구 CA 제거 순서로 한다.
- missing, expired, untrusted, extra/wrong-role/wrong-cluster URI SAN은 거부한다. CN fallback은 없다.

## 장애 대응

| 상황 | 기대 동작 | 조치 |
|---|---|---|
| Agent A 단절 | A stale 후 503; B 정상 | A egress/cert 확인 후 snapshot commit 확인 |
| gap/flood | A만 Nack/resync/rate-limit | A cap/queue 지표 확인 |
| Registry 재시작 | API ready false, Agent full resync | 모든 cluster commit 확인 |
| API replica 재시작 | Watch snapshot으로 local catalog 복구 | SSE reset 1건 확인 |
| 인증서 폐기 | 해당 역할 연결 거부 | CA 배포 순서와 SAN 확인 |

## 용량과 롤백

Registry 운영 상한은 stale TTL 24시간, heartbeat timeout 1시간(항상 stale TTL 이하), ingress frame rate 초당 100,000개, ingress byte rate 초당 1 GiB이다. 이 값은 폭주 격리와 stale 데이터 회수 시간을 유계로 유지하기 위한 안전 상한이며, 초과·NaN·Inf·파싱 불가 값은 리스너 생성 전에 거부한다.
Registry ingress byte burst는 최대 protocol message 크기 이상이어야 한다. Agent와 Registry가 별도 chart이므로 운영자는 두 chart의 maximum message 값을 동일하게 유지해야 한다.
Agent snapshot chunk는 4 MiB와 1,000 resources 중 먼저 도달한 경계에서 나눈다. 별도 chart의 Agent `maxChunkResources`는 중앙 Registry 값 이하로 유지한다.

기본 상한은 cluster당 100,000 resources/256 MiB, frame 4 MiB, global retained 512 MiB다. Registry memory limit 1536 MiB는 staging/map/serialization headroom을 포함한다. 상한 변경 전 100k fixture의 snapshot/delta/query/Watch peak RSS를 다시 측정한다.

롤백은 Agent 중지 → direct API의 기존 Kubernetes 권한 복구 → `clusterState.mode=direct` manifest 기준선 diff0 확인 → API rollout/informer sync → Registry 제거 순서다. Registry는 메모리 전용이라 보존 데이터가 없다.
