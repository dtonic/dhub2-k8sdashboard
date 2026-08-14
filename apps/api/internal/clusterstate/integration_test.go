//go:build integration

// 실제 kube-apiserver를 대상으로 하는 테스트입니다.
//
//	make api-itest                     # 로컬 kube-apiserver를 띄워서 실행
//	ITEST_KUBECONFIG=~/.kube/config make api-itest   # 실제 클러스터(예: lnode)
//
// **기본 동작은 읽기 전용입니다.** 운영 클러스터에 겨눠도 아무것도 만들지 않습니다.
// 상태 반영 지연 측정처럼 객체를 만들어야 하는 테스트는 ITEST_MUTATE=1일 때만 돕니다.
package clusterstate_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	authzv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/cache"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/clusterstate"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/contract"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/datasource/demo"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/httpapi"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/scope"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/testcluster"
)

const liveClusterID = "itest"

func liveOptions() clusterstate.Options {
	return clusterstate.Options{
		Resync:             10 * time.Minute,
		EventFieldSelector: "type=Warning",
		ClusterID:          liveClusterID,
		ClusterName:        "Integration Test Cluster",
	}
}

// setupLive는 실서버에 붙은 Store와 요청 계수기를 준비합니다.
// 한 테스트 안에서 Live를 두 번 부르면 로컬 API 서버가 두 번 뜨므로 여기서만 부릅니다.
func setupLive(t *testing.T) (context.Context, *clusterstate.Store, *testcluster.Counter, *rest.Config) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	cfg, counter := testcluster.Live(t)
	store := testcluster.LiveStore(t, ctx, cfg, liveOptions())
	return ctx, store, counter, cfg
}

/* ── ADR 0004의 주장을 실서버에서 확인 ─────────────────────────────────── */

func TestLiveProtobufIsActuallyNegotiated(t *testing.T) {
	// ADR 0004에서 Go를 고른 **1순위 근거**입니다. 설정만 넣어두고 실제로는
	// JSON으로 오고 있으면, 그 결정의 실익이 통째로 사라집니다.
	_, _, counter, _ := setupLive(t)

	var proto, json, other int
	for _, r := range counter.All() {
		switch {
		case strings.Contains(r.ContentType, "vnd.kubernetes.protobuf"):
			proto++
		case strings.Contains(r.ContentType, "application/json"):
			json++
		default:
			other++
		}
	}
	t.Logf("응답 Content-Type — protobuf %d · json %d · 기타 %d", proto, json, other)

	if proto == 0 {
		for _, r := range counter.All() {
			t.Logf("  %s %s → %s", r.Method, r.Path, r.ContentType)
		}
		t.Fatal("protobuf로 온 응답이 하나도 없습니다. 콘텐츠 협상이 적용되지 않았습니다")
	}
	// 내장 타입 LIST는 전부 protobuf여야 합니다. metadata informer와 watch 스트림은
	// 자체 협상 규칙을 쓰므로 여기서 세지 않습니다.
	for _, r := range counter.All() {
		if r.Method != http.MethodGet || strings.Contains(r.Query, "watch=true") {
			continue
		}
		if strings.Contains(r.Path, "/api/v1/pods") && !strings.Contains(r.ContentType, "protobuf") {
			t.Errorf("Pod LIST가 protobuf가 아닙니다: %s", r.ContentType)
		}
	}
}

func TestLiveInitialSyncIsOneListPlusOneWatchPerResource(t *testing.T) {
	// 폴링이 섞여 들어오면 사용자가 없어도 클러스터 부하가 상수로 발생합니다.
	_, _, counter, _ := setupLive(t)

	lists, watches := map[string]int{}, map[string]int{}
	for _, r := range counter.All() {
		if r.Method != http.MethodGet || !strings.HasPrefix(r.Path, "/api") {
			continue
		}
		key := r.Path
		if strings.Contains(r.Query, "watch=true") {
			watches[key]++
		} else {
			lists[key]++
		}
	}
	if len(lists) == 0 {
		t.Fatal("LIST가 한 번도 없었습니다")
	}
	for path, n := range lists {
		if n > 1 {
			t.Errorf("%s: LIST %d회 — 최초 1회여야 합니다", path, n)
		}
		if watches[path] == 0 {
			t.Errorf("%s: WATCH가 열리지 않았습니다. 폴링으로 동작할 위험이 있습니다", path)
		}
	}
	t.Logf("리소스 %d종 · LIST %d회 · WATCH %d회", len(lists), sum(lists), sum(watches))
}

func TestLiveServingCausesZeroAPICalls(t *testing.T) {
	// **이 테스트가 이 파일의 핵심입니다.** 화면을 아무리 많이 그려도
	// API 서버로 나가는 요청이 늘지 않아야 합니다.
	ctx, store, counter, _ := setupLive(t)

	src := demo.New(store)
	srv := httpapi.NewServer(httpapi.Deps{
		Store: store, Metrics: src, Logs: src, Alerts: src, Topology: src,
		Resolver: scope.Static{S: scope.Scope{Clusters: []scope.Cluster{{
			ID: liveClusterID, Name: "Integration Test Cluster", All: true,
		}}}},
		// 캐시가 요청을 가려서 0회가 나오면 의미가 없습니다. TTL을 없앱니다.
		Cache: cache.NewTTL(time.Nanosecond),
	})

	ns := anyNamespace(t, store)
	paths := []string{
		"/api/v1/scope",
		"/api/v1/clusters/" + liveClusterID + "/overview?range=1h",
		"/api/v1/clusters/" + liveClusterID + "/namespaces?range=1h",
		"/api/v1/clusters/" + liveClusterID + "/namespaces/" + ns + "?range=1h",
		"/api/v1/clusters/" + liveClusterID + "/logs?range=1h",
		"/api/v1/clusters/" + liveClusterID + "/topology?range=1h",
		"/api/v1/clusters/" + liveClusterID + "/alerts?range=7d",
	}

	// watch 스트림이 자리를 잡을 시간을 준 뒤 기준선을 잡습니다.
	time.Sleep(2 * time.Second)
	before := counter.Len()

	for round := 0; round < 3; round++ {
		for _, p := range paths {
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil).WithContext(ctx))
			if rec.Code != http.StatusOK {
				t.Errorf("%s → %d\n%s", p, rec.Code, truncate(rec.Body.String(), 300))
			}
		}
	}

	extra := counter.Since(before)
	if len(extra) != 0 {
		for _, r := range extra {
			t.Logf("  추가 호출: %s %s?%s", r.Method, r.Path, r.Query)
		}
		t.Fatalf("화면 %d회를 그리는 동안 API 서버 호출이 %d회 발생했습니다 (want 0)",
			len(paths)*3, len(extra))
	}
	t.Logf("화면 %d회 · API 서버 추가 호출 0회", len(paths)*3)
}

func TestLiveEventWatchIsNarrowedServerSide(t *testing.T) {
	// Event는 대부분의 클러스터에서 수가 가장 많은 리소스입니다.
	// 전부 받아와서 우리가 거르면, 줄인 것은 우리 메모리뿐이고 클러스터 부하는 그대로입니다.
	_, store, counter, _ := setupLive(t)

	narrowed := false
	for _, r := range counter.All() {
		if strings.Contains(r.Path, "/events") && strings.Contains(r.Query, "fieldSelector") {
			narrowed = true
			t.Logf("Event 요청: %s?%s", r.Path, r.Query)
		}
	}
	if !narrowed {
		t.Error("Event 요청에 fieldSelector가 없습니다. 전체 Event를 watch하고 있습니다")
	}

	evs, err := store.Events(clusterstate.NamespaceFilter{All: true}, time.Now().Add(-30*24*time.Hour), 500)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range evs {
		if e.Type != "Warning" {
			t.Errorf("Warning만 watch했는데 %s 이벤트가 캐시에 있습니다: %s", e.Type, e.Reason)
			break
		}
	}
	t.Logf("캐시된 Warning 이벤트 %d건", len(evs))
}

/* ── 실제 데이터로 정규화 검증 ─────────────────────────────────────────── */

func TestLiveEveryPodNormalizesToAKnownState(t *testing.T) {
	// 픽스처는 우리가 상상한 상태만 담습니다. 실클러스터에는 우리가 안 만든 조합이 있습니다.
	_, store, _, _ := setupLive(t)

	pods := store.CatalogPods("", 0)
	if len(pods) == 0 {
		t.Skip("클러스터에 Pod가 없습니다")
	}

	valid := map[contract.Severity]bool{
		contract.SeverityHealthy: true, contract.SeverityProgressing: true,
		contract.SeverityWarning: true, contract.SeverityDegraded: true,
		contract.SeverityCritical: true, contract.SeverityUnknown: true,
	}
	bySeverity := map[contract.Severity]int{}
	noWorkload := 0

	for _, cp := range pods {
		pod, found, err := store.Pod(cp.Namespace, cp.Name, cp.UID)
		if err != nil || !found {
			t.Fatalf("카탈로그의 Pod를 캐시에서 찾을 수 없습니다: %s/%s", cp.Namespace, cp.Name)
		}
		s := store.PodSummary(pod)
		if !valid[s.Severity] {
			t.Errorf("%s/%s: 알 수 없는 severity %q", s.Namespace, s.Name, s.Severity)
		}
		if s.Ready == "" || !strings.Contains(s.Ready, "/") {
			t.Errorf("%s/%s: ready 표기가 이상합니다 %q", s.Namespace, s.Name, s.Ready)
		}
		if s.Issues == nil {
			t.Errorf("%s/%s: issues가 nil입니다. JSON에서 null이 됩니다", s.Namespace, s.Name)
		}
		if s.UID == "" {
			t.Errorf("%s/%s: UID가 비었습니다", s.Namespace, s.Name)
		}
		if cp.WorkloadName == "" {
			noWorkload++
		}
		bySeverity[s.Severity]++
	}
	t.Logf("Pod %d개 정규화 · %v", len(pods), bySeverity)
	if noWorkload > 0 {
		t.Logf("소유 워크로드를 못 찾은 Pod %d개 (static pod 등이면 정상입니다)", noWorkload)
	}
}

func TestLiveOwnerChainMatchesRealCluster(t *testing.T) {
	_, store, _, _ := setupLive(t)

	workloads, err := store.Workloads(clusterstate.NamespaceFilter{All: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(workloads) == 0 {
		t.Skip("클러스터에 워크로드가 없습니다")
	}

	checked := 0
	for _, w := range workloads {
		pods, err := store.PodsForWorkload(w.Namespace, w.Kind, w.Name, w.Ref.WorkloadUID)
		if err != nil {
			t.Fatalf("%s/%s: %v", w.Namespace, w.Name, err)
		}
		for _, p := range pods {
			// ReplicaSet은 구현 세부사항입니다. 화면에는 Deployment로 올라와야 합니다.
			if p.Ref.WorkloadKind != w.Kind || p.Ref.WorkloadName != w.Name {
				t.Errorf("%s %s/%s의 Pod가 다른 워크로드를 가리킵니다: %+v", w.Kind, w.Namespace, w.Name, p.Ref)
			}
		}
		if w.Kind == "Deployment" && len(pods) > 0 {
			chain := store.WorkloadOwnerChain(w.Namespace, w.Kind, w.Name, w.Ref.WorkloadUID)
			if len(chain) == 0 {
				t.Errorf("Deployment %s/%s에 ReplicaSet 체인이 없습니다", w.Namespace, w.Name)
				continue
			}
			current := 0
			for _, c := range chain {
				if c.Current {
					current++
				}
			}
			if current != 1 {
				t.Errorf("%s/%s: 현재 세대가 %d개입니다 (want 1): %+v", w.Namespace, w.Name, current, chain)
			}
			checked++
		}
	}
	t.Logf("워크로드 %d개 · Deployment 세대 체인 %d개 확인", len(workloads), checked)
}

func TestLiveScopeNeverLeaksAcrossNamespaces(t *testing.T) {
	_, store, _, _ := setupLive(t)

	ns := anyNamespace(t, store)
	f := clusterstate.NamespaceFilter{List: []string{ns}}

	summaries, err := store.NamespaceSummaries(f)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range summaries {
		if s.Name != ns {
			t.Errorf("범위 밖 namespace가 섞였습니다: %s", s.Name)
		}
	}
	ws, err := store.Workloads(f)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range ws {
		if w.Namespace != ns {
			t.Errorf("범위 밖 워크로드가 섞였습니다: %s/%s", w.Namespace, w.Name)
		}
	}
	t.Logf("%s로 좁힌 결과 — namespace %d · workload %d", ns, len(summaries), len(ws))
}

/* ── 상태 반영 지연 (이슈 #8 완료 기준) ───────────────────────────────── */

func TestLivePodStateChangeReachesCacheQuickly(t *testing.T) {
	// 이슈 #8의 "Pod 상태 변경이 정의된 지연 시간 안에 반영됨"입니다.
	// 객체를 만들어야 측정되므로 명시적으로 켤 때만 돕니다.
	if os.Getenv("ITEST_MUTATE") != "1" {
		t.Skip("객체를 생성하는 테스트입니다. ITEST_MUTATE=1로 실행하세요.")
	}
	ctx, store, _, cfg := setupLive(t)
	cs := testcluster.Clientset(t, cfg)

	nsName := fmt.Sprintf("k8s-dashboard-itest-%d", time.Now().UnixNano()%100000)
	if _, err := cs.CoreV1().Namespaces().Create(ctx,
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName}}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("테스트 namespace 생성 실패: %v", err)
	}
	t.Cleanup(func() {
		_ = cs.CoreV1().Namespaces().Delete(context.Background(), nsName, metav1.DeleteOptions{})
	})

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "probe", Namespace: nsName},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "app", Image: "registry.k8s.io/pause:3.9",
		}}},
	}
	start := time.Now()
	created, err := cs.CoreV1().Pods(nsName).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Pod 생성 실패: %v", err)
	}
	uid := string(created.UID)

	appeared := waitFor(t, 30*time.Second, func() bool {
		_, found, _ := store.Pod(nsName, "probe", uid)
		return found
	})
	if !appeared {
		t.Fatal("30초 안에 Pod가 캐시에 나타나지 않았습니다. watch가 동작하지 않습니다")
	}
	create := time.Since(start)

	// 라벨 하나만 바꿔도 watch 이벤트가 발생합니다. 스케줄링 결과를 기다리지 않아
	// kubelet이 없는 환경에서도 측정됩니다.
	start = time.Now()
	patched, err := cs.CoreV1().Pods(nsName).Get(ctx, "probe", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Pod 조회 실패: %v", err)
	}
	if patched.Labels == nil {
		patched.Labels = map[string]string{}
	}
	patched.Labels["itest-generation"] = "2"
	if _, err := cs.CoreV1().Pods(nsName).Update(ctx, patched, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("Pod 갱신 실패: %v", err)
	}
	updated := waitFor(t, 30*time.Second, func() bool {
		p, found, _ := store.Pod(nsName, "probe", uid)
		return found && p.Labels["itest-generation"] == "2"
	})
	if !updated {
		t.Fatal("30초 안에 변경이 캐시에 반영되지 않았습니다")
	}
	update := time.Since(start)

	t.Logf("생성 → 캐시 반영 %v · 변경 → 캐시 반영 %v", create.Round(time.Millisecond), update.Round(time.Millisecond))
	// 사람이 새로고침해서 알아채기 전에 반영되어야 합니다.
	if update > 5*time.Second {
		t.Errorf("변경 반영이 %v 걸렸습니다. 5초를 넘으면 화면이 낡은 값을 보여줍니다", update)
	}
}

/* ── 최소 권한 (이슈 #8 완료 기준) ────────────────────────────────────── */

func TestLiveDeployedServiceAccountCannotReadSecrets(t *testing.T) {
	// deploy/rbac/의 매니페스트를 적용한 뒤 실행합니다.
	//   ITEST_SERVICE_ACCOUNT=k8s-dashboard:k8s-dashboard-api
	sa := os.Getenv("ITEST_SERVICE_ACCOUNT")
	if sa == "" {
		t.Skip("ITEST_SERVICE_ACCOUNT=<namespace>:<name> 을 설정하면 실제 권한을 확인합니다.")
	}
	parts := strings.SplitN(sa, ":", 2)
	if len(parts) != 2 {
		t.Fatalf("ITEST_SERVICE_ACCOUNT 형식은 <namespace>:<name> 입니다: %q", sa)
	}
	user := fmt.Sprintf("system:serviceaccount:%s:%s", parts[0], parts[1])

	ctx := context.Background()
	cfg, _ := testcluster.Live(t)
	cs := testcluster.Clientset(t, cfg)

	type check struct {
		group, resource, verb string
		want                  bool
	}
	checks := []check{
		{"", "pods", "list", true},
		{"", "pods", "watch", true},
		{"", "nodes", "watch", true},
		{"", "events", "watch", true},
		{"apps", "deployments", "watch", true},
		{"apps", "replicasets", "watch", true},
		{"batch", "cronjobs", "watch", true},
		// 여기부터는 **거절되어야** 합니다.
		{"", "secrets", "get", false},
		{"", "secrets", "list", false},
		{"", "configmaps", "list", false},
		{"", "pods", "delete", false},
		{"", "pods/exec", "create", false},
	}

	for _, c := range checks {
		sar := &authzv1.SubjectAccessReview{Spec: authzv1.SubjectAccessReviewSpec{
			User: user,
			ResourceAttributes: &authzv1.ResourceAttributes{
				Group: c.group, Resource: c.resource, Verb: c.verb,
			},
		}}
		res, err := cs.AuthorizationV1().SubjectAccessReviews().Create(ctx, sar, metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("SubjectAccessReview 실패: %v", err)
		}
		name := c.resource + "/" + c.verb
		if res.Status.Allowed != c.want {
			if c.want {
				t.Errorf("%s: 허용되어야 하는데 거절되었습니다 (%s)", name, res.Status.Reason)
			} else {
				t.Errorf("%s: **거절되어야 하는데 허용되었습니다.** 권한이 넓습니다", name)
			}
		}
	}
	t.Logf("%s 권한 검사 %d건 완료", user, len(checks))
}

/* ── 헬퍼 ───────────────────────────────────────────────────────────────── */

func anyNamespace(t *testing.T, store *clusterstate.Store) string {
	t.Helper()
	pods := store.CatalogPods("", 0)
	for _, p := range pods {
		if p.Namespace != "" {
			return p.Namespace
		}
	}
	return "default"
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func sum(m map[string]int) int {
	n := 0
	for _, v := range m {
		n += v
	}
	return n
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
