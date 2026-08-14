//go:build e2efixture

// Command e2efixture는 통합 E2E(#22) 전용 서버입니다. **프로덕션 빌드가 아닙니다** —
// 빌드 태그 `e2efixture` 없이는 컴파일되지 않고, 배포 이미지에도 포함되지 않습니다.
//
// 프로덕션 Web 번들(VITE_USE_MOCK=false dist)과 실제 httpapi를 loopback 오리진
// 하나로 서빙합니다. 데이터는 가짜 informer(testcluster) + 시나리오 corpus입니다.
//
//	go run -tags e2efixture ./cmd/e2efixture -addr 127.0.0.1:4273 -dist ../web/dist
//	go run -tags e2efixture ./cmd/e2efixture -outages greptime   # 메트릭 소스 중단
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/e2efixture"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:4273", "loopback listen 주소")
	dist := flag.String("dist", "../web/dist", "VITE_USE_MOCK=false 프로덕션 번들 경로")
	outages := flag.String("outages", "", "강제 중단할 데이터소스 (greptime|quickwit|alerts, 쉼표 구분)")
	scenarios := flag.String("scenarios", "", "켤 시나리오 (crashloop|imagepull|cpuspike|errorlog, 쉼표 구분 · 비우면 전체)")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	f, err := e2efixture.Start(ctx, e2efixture.Config{
		Addr:      *addr,
		DistDir:   *dist,
		Outages:   splitList(*outages),
		Scenarios: splitList(*scenarios),
	}, logger)
	if err != nil {
		// fail-fast — 잘못 뜬 픽스처는 틀린 화면을 정상처럼 보여줍니다.
		logger.Error("픽스처를 시작하지 못했습니다", "err", err)
		os.Exit(1)
	}
	defer f.Close()

	select {
	case <-ctx.Done():
		logger.Info("종료 신호 수신 · 픽스처를 정리합니다")
	case err, ok := <-f.Errors():
		if ok {
			logger.Error("픽스처 HTTP 서버 실패", "err", err)
			f.Close()
			os.Exit(1)
		}
	}
}

func splitList(v string) []string {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
