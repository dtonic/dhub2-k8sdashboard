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
)
