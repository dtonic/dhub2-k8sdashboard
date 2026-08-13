package httpapi

import (
	"encoding/json"
	"net/http"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/contract"
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	// 여기서 실패하면 이미 헤더가 나간 뒤라 할 수 있는 것이 없습니다.
	_ = json.NewEncoder(w).Encode(v)
}

// writeError는 **화면 전체가 실패**했을 때만 씁니다.
// 섹션 하나의 문제는 Section의 degraded/forbidden으로 표현합니다. (ADR 0002)
func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, contract.APIError{Code: code, Message: message})
}
