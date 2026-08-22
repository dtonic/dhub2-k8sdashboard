package resourcecatalog_test

// 최근 항목 재해석의 경계 검증입니다. (ADR 0023 결정 7)
//
//   - 제목의 근거는 브라우저 저장값이 아니라 **서버 인덱스**에서 나온다.
//   - 삭제·권한 상실·UID 교체는 오류가 아니라 **조용한 제거**다.
//   - 크기·구조 위반은 조용히 자르지 않고 오류다.

import (
	"fmt"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/resourcecatalog"
)

func recentRef(resource, namespace, name, uid string) resourcecatalog.RecentRef {
	return resourcecatalog.RecentRef{Group: "", Version: "v1", Resource: resource, Namespace: namespace, Name: name, UID: uid}
}

func recentHarness(t *testing.T) *harness {
	t.Helper()
	h := start(t, options{
		allowlist: []schema.GroupVersionResource{serviceGVR, storageClassGVR},
		metaObjects: []runtime.Object{
			metaObject("v1", "Service", "allowed", "payments-api", "uid-1", nil),
			metaObject("v1", "Service", "forbidden", "payments-worker", "uid-2", nil),
			metaObject("storage.k8s.io/v1", "StorageClass", "", "fast-ssd", "uid-3", nil),
		},
		tune: searchTuner(resourcecatalog.DefaultMaxSearchIndexBytes),
	})
	waitForState(t, h.svc, serviceGVR, resourcecatalog.StateReady)
	waitForState(t, h.svc, storageClassGVR, resourcecatalog.StateReady)
	return h
}

func TestRecentResolvesTitlesFromTheServerIndex(t *testing.T) {
	h := recentHarness(t)
	items, err := h.svc.Recent([]resourcecatalog.RecentRef{recentRef("services", "allowed", "payments-api", "uid-1")}, allNamespaces())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("항목 1건이어야 합니다: %+v", items)
	}
	got := items[0]
	// kind는 브라우저가 보낸 적이 없습니다 — discovery snapshot에서만 나옵니다.
	if got.Kind != "Service" || got.Group != "core" || !got.Namespaced {
		t.Fatalf("서버가 제목의 근거를 다시 채우지 않았습니다: %+v", got)
	}
	if got.Namespace != "allowed" || got.Name != "payments-api" || got.UID != "uid-1" {
		t.Fatalf("신원이 어긋났습니다: %+v", got)
	}
}

func TestRecentDropsStaleForbiddenAndUnknownRefs(t *testing.T) {
	h := recentHarness(t)
	refs := []resourcecatalog.RecentRef{
		recentRef("services", "allowed", "payments-api", "uid-1"),                                            // 유지
		recentRef("services", "allowed", "deleted-api", "uid-gone"),                                          // 인덱스에 없음
		recentRef("services", "allowed", "payments-api", "uid-other"),                                        // 같은 이름·다른 UID
		recentRef("services", "forbidden", "payments-worker", "uid-2"),                                       // Scope 밖
		recentRef("configmaps", "allowed", "settings", "uid-4"),                                              // allowlist 밖
		{Group: "storage.k8s.io", Version: "v1", Resource: "storageclasses", Name: "fast-ssd", UID: "uid-3"}, // 클러스터 범위
	}
	items, err := h.svc.Recent(refs, resourcecatalog.NamespaceFilter{List: []string{"allowed"}})
	if err != nil {
		t.Fatalf("해석되지 않는 참조는 오류가 아니라 제거여야 합니다: %v", err)
	}
	if len(items) != 1 || items[0].UID != "uid-1" {
		t.Fatalf("살아남아야 할 항목이 하나입니다: %+v", items)
	}

	// 클러스터 전체 권한에서는 클러스터 범위 항목이 다시 보입니다.
	full, err := h.svc.Recent(refs, allNamespaces())
	if err != nil {
		t.Fatal(err)
	}
	if len(full) != 3 {
		t.Fatalf("전체 권한에서 3건(서비스 2 + StorageClass 1)이어야 합니다: %+v", full)
	}
}

func TestRecentPreservesInputOrder(t *testing.T) {
	h := recentHarness(t)
	refs := []resourcecatalog.RecentRef{
		recentRef("services", "forbidden", "payments-worker", "uid-2"),
		recentRef("services", "allowed", "payments-api", "uid-1"),
	}
	items, err := h.svc.Recent(refs, allNamespaces())
	if err != nil {
		t.Fatal(err)
	}
	// 브라우저의 최신순을 서버가 뒤집지 않습니다. 웹이 요청을 나눠 보내도
	// 각 응답이 입력 순서를 지키므로 이어 붙이면 원래 순서가 됩니다.
	if len(items) != 2 || items[0].UID != "uid-2" || items[1].UID != "uid-1" {
		t.Fatalf("입력 순서가 보존되지 않았습니다: %+v", items)
	}
}

func TestRecentRefEncodingRoundTripsAndRejectsGarbage(t *testing.T) {
	ref := recentRef("services", "payments", "payments-api", "uid-1")
	encoded := resourcecatalog.EncodeRecentRef(ref)
	if len(encoded) > resourcecatalog.MaxRecentRefLen {
		t.Fatalf("참조가 %d자입니다 — 상한은 %d입니다", len(encoded), resourcecatalog.MaxRecentRefLen)
	}
	got, err := resourcecatalog.ParseRecentRef(encoded)
	if err != nil || got != ref {
		t.Fatalf("왕복이 깨졌습니다: %+v %v", got, err)
	}
	for _, bad := range []string{
		"",                 // 빈 값
		"!!!not-base64!!!", // base64 아님
		"YWJj",             // 필드 수가 다름
		strings.Repeat("a", resourcecatalog.MaxRecentRefLen+1), // 길이 초과
	} {
		if _, err := resourcecatalog.ParseRecentRef(bad); err != resourcecatalog.ErrInvalidFilter {
			t.Errorf("잘못된 참조 %q를 받아들였습니다: %v", bad, err)
		}
	}
}

func TestRecentRejectsTooManyRefsInsteadOfTruncating(t *testing.T) {
	encoded := make([]string, 0, resourcecatalog.MaxRecentRefs+1)
	for i := 0; i <= resourcecatalog.MaxRecentRefs; i++ {
		encoded = append(encoded, resourcecatalog.EncodeRecentRef(
			recentRef("services", "payments", fmt.Sprintf("payments-%03d", i), fmt.Sprintf("uid-%03d", i))))
	}
	// 조용히 20개만 처리하면 클라이언트는 잘린 줄 모릅니다.
	if _, err := resourcecatalog.ParseRecentRefs(encoded); err != resourcecatalog.ErrInvalidFilter {
		t.Fatalf("상한을 넘은 요청을 받아들였습니다: %v", err)
	}
	if _, err := resourcecatalog.ParseRecentRefs(encoded[:resourcecatalog.MaxRecentRefs]); err != nil {
		t.Fatalf("상한 이내 요청이 거절되었습니다: %v", err)
	}

	h := recentHarness(t)
	tooMany := make([]resourcecatalog.RecentRef, resourcecatalog.MaxRecentRefs+1)
	if _, err := h.svc.Recent(tooMany, allNamespaces()); err != resourcecatalog.ErrInvalidFilter {
		t.Fatalf("서비스 계층도 상한을 강제해야 합니다: %v", err)
	}
}
