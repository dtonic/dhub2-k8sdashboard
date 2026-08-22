package resourcecatalog

import "errors"

// 이 패키지의 실패는 전부 **명시적**입니다. "빈 목록"으로 뭉개지 않습니다. (ADR 0002)
var (
	// ErrUnavailable은 이 배포에 Resource Explorer가 없다는 뜻입니다(central 모드·비활성).
	ErrUnavailable = errors.New("resource explorer is unavailable")
	// ErrNotAllowlisted는 allowlist에 없는 GVR입니다.
	ErrNotAllowlisted = errors.New("resource is not allowlisted")
	// ErrNotServed는 클러스터 discovery에 없는 GVR입니다(미설치 CRD·삭제된 API).
	ErrNotServed = errors.New("resource is not served by the cluster")
	// ErrUnsupported는 PartialObjectMetadata LIST/WATCH를 지원하지 않는 API입니다(406).
	// full-object watch로 물러나지 않고 이 상태를 그대로 노출합니다. (ADR 0018 결정 4)
	ErrUnsupported = errors.New("metadata list/watch is not supported by the api")
	// ErrForbidden은 서버(ServiceAccount)에 해당 리소스 조회 권한이 없다는 뜻입니다.
	// 사용자 권한 부족(403)과 다른 상태입니다.
	ErrForbidden = errors.New("the api server denied list/watch for this resource")
	// ErrSyncing은 캐시가 아직 최초 동기화 중이라는 뜻입니다. 0건과 다릅니다.
	ErrSyncing = errors.New("resource cache is still syncing")
	// ErrInvalidCursor는 형식·지문이 맞지 않는 cursor입니다.
	ErrInvalidCursor = errors.New("cursor is invalid")
	// ErrInvalidFilter는 상한을 넘거나 해석되지 않는 필터입니다.
	ErrInvalidFilter = errors.New("filter is invalid")
	// ErrRateLimited는 상세 조회 rate/동시성 상한 초과입니다.
	ErrRateLimited = errors.New("detail request budget exceeded")
	// ErrObjectNotFound는 요청한 (namespace, name)이 로컬 metadata 인덱스에 없다는 뜻입니다.
	// 상세 조회는 목록에서 실제로 본 행만 열 수 있으므로, 여기서 막히면 API 서버로
	// 나가는 요청 자체가 없습니다.
	ErrObjectNotFound = errors.New("object is not present in the metadata cache")
	// ErrUIDMismatch는 열려 있던 항목이 같은 이름의 다른 객체로 교체된 경우입니다.
	ErrUIDMismatch = errors.New("object uid does not match the requested uid")
	// ErrTooLarge는 상세 객체가 응답 상한을 넘은 경우입니다.
	ErrTooLarge = errors.New("object exceeds the response size limit")
	// ErrSearchDisabled는 이 배포에서 전역 검색을 끈 상태입니다. (ADR 0023 롤백 스위치)
	// Resource Explorer 자체는 그대로 동작하므로 ErrUnavailable과 다른 상태입니다.
	ErrSearchDisabled = errors.New("global resource search is disabled")
	// ErrInvalidQuery는 길이·문자 규칙을 벗어난 검색어입니다.
	// 1자 접두사는 사실상 전체 순회이므로 거절합니다.
	ErrInvalidQuery = errors.New("search query is invalid")
	// ErrSearchTooBroad는 질의가 여는 (리소스, namespace) 스트림이 상한을 넘은 경우입니다.
	// 결과를 조용히 자르는 대신 더 긴 접두사를 요구합니다.
	ErrSearchTooBroad = errors.New("search query matches too many namespaces")
)

// 변경 검토 dry-run (ADR 0019 Phase 1)
//
// 이 경로는 **원본 오류를 절대 밖으로 내보내지 않습니다.** Kubernetes Status 본문에는
// 내부 주소·webhook 이름·매니페스트 조각이 들어 있을 수 있고, 그것이 응답이나 로그로
// 흘러가면 안 됩니다. 그래서 upstream 실패는 전부 아래 sentinel 중 하나로 바뀌고,
// 사용자에게 보여줄 사유는 서버가 새로 쓴 violation 목록으로만 전달됩니다.
//
// U3 핸들러가 붙일 매핑(이 파일이 계약입니다)
//
//	ErrDryRunDisabled           503 dryrun_unavailable
//	ErrDryRunDenied             403 dryrun_resource_denied
//	ErrDryRunForbidden          502 dryrun_forbidden
//	ErrDryRunRateLimited        429 dryrun_rate_limited
//	ErrManifestInvalid          400 invalid_manifest
//	ErrManifestMismatch         400 manifest_mismatch
//	ErrManifestTooLarge         413 manifest_too_large
//	ErrResourceVersionMismatch  409 resource_version_mismatch
//	ErrUIDMismatch              409 uid_mismatch
//	ErrObjectNotFound           404 not_found
//	ErrTooLarge                 502 object_too_large
//	ErrDryRunUpstream           502 upstream_unavailable
//	context.DeadlineExceeded    504 upstream_timeout
var (
	// ErrDryRunDisabled는 이 배포에서 변경 검토가 꺼져 있다는 뜻입니다.
	// 기능 스위치가 false거나 전용 클라이언트가 없을 때(central 모드 포함)입니다.
	ErrDryRunDisabled = errors.New("change review dry-run is disabled")
	// ErrDryRunDenied는 이 GVR이 검토 대상이 아니라는 뜻입니다. hard-deny이거나
	// opt-in 목록에 없습니다. **권한 부족이 아니라 정책**입니다.
	ErrDryRunDenied = errors.New("resource is not eligible for change review")
	// ErrDryRunForbidden은 대시보드 ServiceAccount에 이 리소스의 patch 권한이
	// 없다는 뜻입니다. 사용자 권한 문제가 아니라 배포 RBAC 문제입니다.
	ErrDryRunForbidden = errors.New("the api server denied the dry-run patch")
	// ErrDryRunRateLimited는 검토 전용 rate·동시성 상한 초과입니다.
	// 상세 조회 예산(ErrRateLimited)과 별개입니다.
	ErrDryRunRateLimited = errors.New("dry-run request budget exceeded")
	// ErrManifestInvalid는 파싱·구조 검사에서 걸린 매니페스트입니다.
	// 다중 문서·중복 키·anchor/alias·깊이·노드 수 초과가 전부 여기입니다.
	ErrManifestInvalid = errors.New("manifest is not a single well-formed kubernetes object")
	// ErrManifestMismatch는 본문·경로·실객체의 신원이 어긋난 경우입니다.
	ErrManifestMismatch = errors.New("manifest identity does not match the request target")
	// ErrManifestTooLarge는 설정된 매니페스트 상한 초과입니다. 파싱 전에 걸립니다.
	ErrManifestTooLarge = errors.New("manifest exceeds the configured size limit")
	// ErrResourceVersionMismatch는 검토 기준이 이미 낡았다는 뜻입니다(CAS 실패).
	// 필드 소유권 충돌(200 rejected)과 다릅니다 — 이쪽은 다시 읽어야 합니다.
	ErrResourceVersionMismatch = errors.New("object resourceVersion does not match the requested version")
	// ErrDryRunUpstream은 분류되지 않은 upstream 실패입니다. 원문을 담지 않습니다.
	ErrDryRunUpstream = errors.New("the cluster did not complete the dry-run")
)
