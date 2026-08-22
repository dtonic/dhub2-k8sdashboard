package httpapi

// writeBoundedJSON 단위 검증입니다. (ADR 0019 Phase 1)
//
// 이 검사만 **패키지 내부**에 둡니다. 상한을 넘긴 응답 경로를 실제 핸들러로
// 재현하려면 256KiB짜리 검토 결과를 만들어 내야 하고, 그러려면 프로덕션 상한을
// 낮추는 seam이 필요합니다. 상한을 테스트를 위해 무르게 만드는 것보다, 헬퍼를
// 합성 응답으로 직접 부르는 편이 안전합니다.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/contract"
)

func dryRunBoundedRequest() *http.Request {
	return httptest.NewRequest(http.MethodPost, "/api/v1/clusters/c/resources/core/v1/configmaps/object/dry-run", nil)
}

// TestWriteBoundedJSONWritesNothingBeforeTheSizeCheck — 상한을 넘으면 부분 본문이
// 아니라 고정 오류만 나가야 합니다. 헤더를 먼저 쓰면 되돌릴 수 없습니다.
func TestWriteBoundedJSONWritesNothingBeforeTheSizeCheck(t *testing.T) {
	rec := httptest.NewRecorder()
	huge := map[string]string{"blob": strings.Repeat("x", 64)}
	if writeBoundedJSON(rec, dryRunBoundedRequest(), huge, 8) {
		t.Fatal("상한을 넘겼는데 성공으로 보고했습니다")
	}
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d want 502", rec.Code)
	}
	var apiErr contract.APIError
	if err := json.Unmarshal(rec.Body.Bytes(), &apiErr); err != nil {
		t.Fatalf("오류 응답이 APIError가 아닙니다: %v\n%s", err, rec.Body.String())
	}
	if apiErr.Code != "object_too_large" {
		t.Errorf("code=%q want object_too_large", apiErr.Code)
	}
	// 잘린 원본 조각이 함께 나가면 안 됩니다.
	if strings.Contains(rec.Body.String(), "xxxx") {
		t.Fatalf("부분 본문이 새어 나갔습니다: %s", rec.Body.String())
	}
}

// TestWriteBoundedJSONWritesTheWholeBodyWhenItFits — 상한 안이면 200과 온전한 본문입니다.
func TestWriteBoundedJSONWritesTheWholeBodyWhenItFits(t *testing.T) {
	rec := httptest.NewRecorder()
	payload := map[string]any{"outcome": "changed", "changeCount": 3}
	if !writeBoundedJSON(rec, dryRunBoundedRequest(), payload, contract.MaxDryRunResponseBytes) {
		t.Fatal("상한 안인데 실패로 보고했습니다")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Errorf("Content-Type=%q", got)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("본문이 온전한 JSON이 아닙니다: %v", err)
	}
	if out["outcome"] != "changed" {
		t.Errorf("본문이 잘렸습니다: %s", rec.Body.String())
	}
}

// TestWriteBoundedJSONFailsClosedOnEncodeError — 직렬화할 수 없는 값은 500이고,
// 역시 부분 본문을 내보내지 않습니다.
func TestWriteBoundedJSONFailsClosedOnEncodeError(t *testing.T) {
	rec := httptest.NewRecorder()
	if writeBoundedJSON(rec, dryRunBoundedRequest(), map[string]any{"fn": func() {}}, 1<<20) {
		t.Fatal("직렬화 실패인데 성공으로 보고했습니다")
	}
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d want 500", rec.Code)
	}
}
