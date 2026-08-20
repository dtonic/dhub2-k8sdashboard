package httpapi_test

import (
	"encoding/json"
	stderrors "errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/contract"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/httpapi"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/scope"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/testcluster"
)

func int32p(v int32) *int32 { return &v }

func manageFixture(t *testing.T, admin bool) fixture {
	t.Helper()
	kube := fake.NewSimpleClientset(
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "payments"},
			Spec:       appsv1.DeploymentSpec{Replicas: int32p(2), Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "api"}}},
			Status:     appsv1.DeploymentStatus{ReadyReplicas: 2},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "payments"},
			Type:       corev1.SecretTypeOpaque,
			Data:       map[string][]byte{"PASSWORD": []byte("s3cr3t")},
		},
	)
	return newFixture(t, func(d *httpapi.Deps) {
		d.KubeClient = kube
		s := scope.Scope{Subject: "u", Clusters: []scope.Cluster{{ID: testcluster.ClusterID, Name: "Seoul", All: true}}}
		if admin {
			s.CanManageWorkloads = true
		}
		d.Resolver = scope.Static{S: s}
	})
}

// TestManageRequiresPermission — 관리 권한이 없으면 모든 관리 엔드포인트가 403.
func TestManageRequiresPermission(t *testing.T) {
	f := manageFixture(t, false)
	base := "/api/v1/clusters/" + testcluster.ClusterID
	for _, url := range []string{base + "/deployments", base + "/secrets", base + "/secrets/payments/db"} {
		rec := httptest.NewRecorder()
		f.srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s viewer = %d, want 403", url, rec.Code)
		}
	}
}

// TestManageUnavailableWithoutClient — KubeClient가 없으면 503(admin이어도).
func TestManageUnavailableWithoutClient(t *testing.T) {
	f := newFixture(t, func(d *httpapi.Deps) {
		d.Resolver = scope.Static{S: scope.Scope{Subject: "a", CanManageWorkloads: true, Clusters: []scope.Cluster{{ID: testcluster.ClusterID, All: true}}}}
	})
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/clusters/"+testcluster.ClusterID+"/deployments", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no client = %d, want 503", rec.Code)
	}
}

// TestSecretValueExposedToAdmin — admin은 Secret 값을 평문으로 받는다(ADR 0014).
func TestSecretValueExposedToAdmin(t *testing.T) {
	f := manageFixture(t, true)
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/clusters/"+testcluster.ClusterID+"/secrets/payments/db", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("admin secret get = %d %s", rec.Code, rec.Body.String())
	}
	var out contract.ManagedSecretDetail
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Data["PASSWORD"] != "s3cr3t" {
		t.Fatalf("평문 값이 노출되지 않았습니다: %+v", out.Data)
	}
	// 목록은 값을 싣지 않아야 한다.
	rec = httptest.NewRecorder()
	f.srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/clusters/"+testcluster.ClusterID+"/secrets", nil))
	if strings.Contains(rec.Body.String(), "s3cr3t") {
		t.Fatal("Secret 목록에 값이 노출되었습니다")
	}
}

// TestDeploymentRestartPatchesTemplate — 재배포가 template annotation을 갱신한다.
func TestDeploymentRestartPatchesTemplate(t *testing.T) {
	f := manageFixture(t, true)
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/clusters/"+testcluster.ClusterID+"/deployments/payments/api/restart", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("restart = %d %s", rec.Code, rec.Body.String())
	}
	var res contract.ManagedActionResult
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil || !res.OK {
		t.Fatalf("restart result = %s", rec.Body.String())
	}
}

/* ── 핸들러 본문 테스트 (#5) ──────────────────────────────────────────────
   guard 테스트가 못 미치는 목록/상세/수정/재배포 본문과 참조 탐색 헬퍼를
   fake clientset으로 검증합니다. */

// manageClientFixture는 임의 scope·객체로 관리 API fixture를 만든다.
func manageClientFixture(t *testing.T, sc scope.Scope, objs ...runtime.Object) (fixture, *fake.Clientset) {
	t.Helper()
	kube := fake.NewSimpleClientset(objs...)
	f := newFixture(t, func(d *httpapi.Deps) {
		d.KubeClient = kube
		d.Resolver = scope.Static{S: sc}
	})
	return f, kube
}

// adminScope는 관리 권한이 있는 Scope를 만든다. namespace 없이 호출하면 전체 접근.
func adminScope(namespaces ...string) scope.Scope {
	cl := scope.Cluster{ID: testcluster.ClusterID, Name: "Seoul"}
	if len(namespaces) == 0 {
		cl.All = true
	} else {
		cl.Namespaces = namespaces
	}
	return scope.Scope{Subject: "admin", CanManageWorkloads: true, Clusters: []scope.Cluster{cl}}
}

func deployment(ns, name string, replicas *int32, spec corev1.PodSpec) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: appsv1.DeploymentSpec{
			Replicas: replicas,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
			Template: corev1.PodTemplateSpec{Spec: spec},
		},
	}
}

func (f fixture) manage(t *testing.T, method, path string, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rd *strings.Reader
	if body == "" {
		rd = strings.NewReader("")
	} else {
		rd = strings.NewReader(body)
	}
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, httptest.NewRequest(method, "/api/v1/clusters/"+testcluster.ClusterID+path, rd))
	return rec
}

// TestDeploymentListScopesAndSorts — 목록이 namespace Scope를 강제하고 ns/name 순으로 정렬한다.
func TestDeploymentListScopesAndSorts(t *testing.T) {
	objs := []runtime.Object{
		deployment("payments", "billing", nil, corev1.PodSpec{}), // Replicas nil → Desired 0
		deployment("payments", "api", int32p(2), corev1.PodSpec{}),
		deployment("ops", "agent", int32p(1), corev1.PodSpec{}),
	}

	t.Run("전체 접근이면 모든 namespace가 정렬되어 나온다", func(t *testing.T) {
		f, _ := manageClientFixture(t, adminScope(), objs...)
		rec := f.manage(t, http.MethodGet, "/deployments", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("list = %d %s", rec.Code, rec.Body.String())
		}
		var out contract.ManagedWorkloadListResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, it := range out.Items {
			got = append(got, it.Namespace+"/"+it.Name)
		}
		want := []string{"ops/agent", "payments/api", "payments/billing"}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("정렬/스코프 결과 = %v, want %v", got, want)
		}
		if out.Items[2].Desired != 0 {
			t.Fatalf("Replicas nil인 Deployment의 Desired = %d, want 0", out.Items[2].Desired)
		}
	})

	t.Run("여러 namespace Scope는 전체 목록 후 필터한다", func(t *testing.T) {
		f, _ := manageClientFixture(t, adminScope("payments", "checkout"), objs...)
		rec := f.manage(t, http.MethodGet, "/deployments", "")
		var out contract.ManagedWorkloadListResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if len(out.Items) != 2 || out.Items[0].Namespace != "payments" {
			t.Fatalf("scope 필터 결과 = %+v", out.Items)
		}
	})

	t.Run("단일 namespace Scope는 그 namespace만 조회한다", func(t *testing.T) {
		f, _ := manageClientFixture(t, adminScope("ops"), objs...)
		rec := f.manage(t, http.MethodGet, "/deployments", "")
		var out contract.ManagedWorkloadListResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if len(out.Items) != 1 || out.Items[0].Name != "agent" {
			t.Fatalf("단일 scope 결과 = %+v", out.Items)
		}
	})

	t.Run("Scope 밖 namespace 요청은 403", func(t *testing.T) {
		f, _ := manageClientFixture(t, adminScope("payments"), objs...)
		if rec := f.manage(t, http.MethodGet, "/deployments?ns=ops", ""); rec.Code != http.StatusForbidden {
			t.Fatalf("scope 밖 ns 목록 = %d, want 403", rec.Code)
		}
	})

	t.Run("목록 조회 실패는 502", func(t *testing.T) {
		f, kube := manageClientFixture(t, adminScope(), objs...)
		kube.PrependReactor("list", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, stderrors.New("apiserver down")
		})
		if rec := f.manage(t, http.MethodGet, "/deployments", ""); rec.Code != http.StatusBadGateway {
			t.Fatalf("목록 실패 = %d, want 502", rec.Code)
		}
	})

	t.Run("Scope에 없는 클러스터는 403", func(t *testing.T) {
		f, _ := manageClientFixture(t, adminScope(), objs...)
		rec := httptest.NewRecorder()
		f.srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/clusters/unknown/deployments", nil))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("미허용 클러스터 = %d, want 403", rec.Code)
		}
	})
}

// TestDeploymentDetailSanitizesManifest — 상세가 서버 관리 필드를 제거한 매니페스트와
// selector로 찾은 Pod 목록을 싣는다.
func TestDeploymentDetailSanitizesManifest(t *testing.T) {
	d := deployment("payments", "api", int32p(2), corev1.PodSpec{})
	d.ResourceVersion = "42"
	d.Annotations = map[string]string{"kubectl.kubernetes.io/last-applied-configuration": "{...}", "team": "pay"}
	d.Status = appsv1.DeploymentStatus{ReadyReplicas: 2, Replicas: 2}
	pods := []runtime.Object{
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "api-1", Namespace: "payments", UID: "uid-1", Labels: map[string]string{"app": "api"}},
			Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{
				{Ready: true, RestartCount: 2}, {Ready: false, RestartCount: 1},
			}},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "api-0", Namespace: "payments", UID: "uid-0", Labels: map[string]string{"app": "api"}},
			Status:     corev1.PodStatus{Phase: corev1.PodPending},
		},
		&corev1.Pod{ // selector 밖 — 결과에 나오면 안 됨
			ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "payments", UID: "uid-x", Labels: map[string]string{"app": "other"}},
			Status:     corev1.PodStatus{Phase: corev1.PodRunning},
		},
	}
	f, _ := manageClientFixture(t, adminScope(), append(pods, d)...)

	rec := f.manage(t, http.MethodGet, "/deployments/payments/api", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("detail = %d %s", rec.Code, rec.Body.String())
	}
	var out contract.ManagedDeploymentDetail
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	for _, banned := range []string{"managedFields", "last-applied-configuration", "\"resourceVersion\""} {
		if strings.Contains(out.Manifest, banned) {
			t.Fatalf("매니페스트에 서버 관리 필드가 남았습니다: %s", banned)
		}
	}
	if !strings.Contains(out.Manifest, `"team": "pay"`) {
		t.Fatalf("사용자 annotation이 사라졌습니다:\n%s", out.Manifest)
	}
	if strings.Contains(out.Manifest, "readyReplicas") {
		t.Fatal("status가 비워지지 않았습니다")
	}
	if len(out.Pods) != 2 || out.Pods[0].Name != "api-0" || out.Pods[1].Name != "api-1" {
		t.Fatalf("selector Pod 목록 = %+v", out.Pods)
	}
	if out.Pods[0].Severity != contract.SeverityWarning || out.Pods[0].Ready {
		t.Fatalf("Pending Pod 상태 해석 = %+v", out.Pods[0])
	}
	if out.Pods[1].Restarts != 3 || !out.Pods[1].Ready {
		t.Fatalf("Running Pod 상태 해석 = %+v", out.Pods[1])
	}

	t.Run("없는 Deployment는 404", func(t *testing.T) {
		if rec := f.manage(t, http.MethodGet, "/deployments/payments/ghost", ""); rec.Code != http.StatusNotFound {
			t.Fatalf("missing detail = %d, want 404", rec.Code)
		}
	})
	t.Run("Scope 밖 namespace 상세는 403", func(t *testing.T) {
		fScoped, _ := manageClientFixture(t, adminScope("payments"), d)
		if rec := fScoped.manage(t, http.MethodGet, "/deployments/ops/agent", ""); rec.Code != http.StatusForbidden {
			t.Fatalf("scope 밖 상세 = %d, want 403", rec.Code)
		}
	})
}

// TestDeploymentUpdateValidatesAndWrites — 수정이 본문·매니페스트를 검증하고 결과를 반영한다.
func TestDeploymentUpdateValidatesAndWrites(t *testing.T) {
	base := deployment("payments", "api", int32p(2), corev1.PodSpec{})

	manifestBody := func(t *testing.T, d *appsv1.Deployment) string {
		t.Helper()
		raw, err := json.Marshal(d)
		if err != nil {
			t.Fatal(err)
		}
		body, err := json.Marshal(map[string]string{"manifest": string(raw)})
		if err != nil {
			t.Fatal(err)
		}
		return string(body)
	}

	t.Run("정상 수정은 200이고 클러스터에 반영된다", func(t *testing.T) {
		f, kube := manageClientFixture(t, adminScope(), base.DeepCopy())
		want := base.DeepCopy()
		want.Spec.Replicas = int32p(5)
		rec := f.manage(t, http.MethodPut, "/deployments/payments/api", manifestBody(t, want))
		if rec.Code != http.StatusOK {
			t.Fatalf("update = %d %s", rec.Code, rec.Body.String())
		}
		got, err := kube.AppsV1().Deployments("payments").Get(t.Context(), "api", metav1.GetOptions{})
		if err != nil || *got.Spec.Replicas != 5 {
			t.Fatalf("반영 결과 = %+v, err=%v", got.Spec.Replicas, err)
		}
	})

	t.Run("본문이 JSON이 아니면 400", func(t *testing.T) {
		f, _ := manageClientFixture(t, adminScope(), base.DeepCopy())
		if rec := f.manage(t, http.MethodPut, "/deployments/payments/api", "not-json"); rec.Code != http.StatusBadRequest {
			t.Fatalf("invalid body = %d, want 400", rec.Code)
		}
	})

	t.Run("매니페스트가 JSON이 아니면 400", func(t *testing.T) {
		f, _ := manageClientFixture(t, adminScope(), base.DeepCopy())
		if rec := f.manage(t, http.MethodPut, "/deployments/payments/api", `{"manifest":"{broken"}`); rec.Code != http.StatusBadRequest {
			t.Fatalf("invalid manifest = %d, want 400", rec.Code)
		}
	})

	t.Run("경로와 name/namespace가 다르면 400", func(t *testing.T) {
		f, _ := manageClientFixture(t, adminScope(), base.DeepCopy())
		other := base.DeepCopy()
		other.Name = "impostor"
		if rec := f.manage(t, http.MethodPut, "/deployments/payments/api", manifestBody(t, other)); rec.Code != http.StatusBadRequest {
			t.Fatalf("manifest mismatch = %d, want 400", rec.Code)
		}
	})

	gr := schema.GroupResource{Group: "apps", Resource: "deployments"}
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"충돌은 409", apierrors.NewConflict(gr, "api", stderrors.New("stale")), http.StatusConflict},
		{"권한 거부는 403", apierrors.NewForbidden(gr, "api", stderrors.New("rbac")), http.StatusForbidden},
		{"유효성 실패는 400", apierrors.NewInvalid(schema.GroupKind{Group: "apps", Kind: "Deployment"}, "api", nil), http.StatusBadRequest},
		{"대상 소실은 404", apierrors.NewNotFound(gr, "api"), http.StatusNotFound},
		{"그 밖의 실패는 502", stderrors.New("boom"), http.StatusBadGateway},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, kube := manageClientFixture(t, adminScope(), base.DeepCopy())
			kube.PrependReactor("update", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, tc.err
			})
			rec := f.manage(t, http.MethodPut, "/deployments/payments/api", manifestBody(t, base.DeepCopy()))
			if rec.Code != tc.want {
				t.Fatalf("update error = %d, want %d (%s)", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

// TestSecretUpdateRewritesData — Secret 수정이 값을 통째로 교체하고 StringData를 비운다.
func TestSecretUpdateRewritesData(t *testing.T) {
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "payments"},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{"PASSWORD": []byte("old"), "STALE": []byte("gone")},
	}

	t.Run("정상 수정은 200이고 값이 교체된다", func(t *testing.T) {
		f, kube := manageClientFixture(t, adminScope(), sec.DeepCopy())
		rec := f.manage(t, http.MethodPut, "/secrets/payments/db", `{"data":{"PASSWORD":"new"}}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("secret update = %d %s", rec.Code, rec.Body.String())
		}
		got, err := kube.CoreV1().Secrets("payments").Get(t.Context(), "db", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if string(got.Data["PASSWORD"]) != "new" || len(got.Data) != 1 || got.StringData != nil {
			t.Fatalf("교체 결과 = %+v", got.Data)
		}
	})

	t.Run("본문이 JSON이 아니면 400", func(t *testing.T) {
		f, _ := manageClientFixture(t, adminScope(), sec.DeepCopy())
		if rec := f.manage(t, http.MethodPut, "/secrets/payments/db", "broken"); rec.Code != http.StatusBadRequest {
			t.Fatalf("invalid body = %d, want 400", rec.Code)
		}
	})

	t.Run("없는 Secret은 404", func(t *testing.T) {
		f, _ := manageClientFixture(t, adminScope(), sec.DeepCopy())
		if rec := f.manage(t, http.MethodPut, "/secrets/payments/ghost", `{"data":{}}`); rec.Code != http.StatusNotFound {
			t.Fatalf("missing secret = %d, want 404", rec.Code)
		}
	})

	t.Run("쓰기 실패는 502", func(t *testing.T) {
		f, kube := manageClientFixture(t, adminScope(), sec.DeepCopy())
		kube.PrependReactor("update", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, stderrors.New("boom")
		})
		if rec := f.manage(t, http.MethodPut, "/secrets/payments/db", `{"data":{}}`); rec.Code != http.StatusBadGateway {
			t.Fatalf("write error = %d, want 502", rec.Code)
		}
	})
}

// TestSecretListScopesNamespaces — Secret 목록도 Deployment 목록과 같은 Scope 규칙을 따른다.
func TestSecretListScopesNamespaces(t *testing.T) {
	objs := []runtime.Object{
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "payments"}, Type: corev1.SecretTypeOpaque},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "tls", Namespace: "ops"}, Type: corev1.SecretTypeTLS},
	}

	t.Run("여러 namespace Scope는 목록을 필터한다", func(t *testing.T) {
		f, _ := manageClientFixture(t, adminScope("payments", "checkout"), objs...)
		rec := f.manage(t, http.MethodGet, "/secrets", "")
		var out contract.ManagedWorkloadListResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if len(out.Items) != 1 || out.Items[0].Name != "db" || out.Items[0].SecretType != string(corev1.SecretTypeOpaque) {
			t.Fatalf("secret scope 필터 = %+v", out.Items)
		}
	})

	t.Run("Scope 밖 namespace 요청은 403", func(t *testing.T) {
		f, _ := manageClientFixture(t, adminScope("payments"), objs...)
		if rec := f.manage(t, http.MethodGet, "/secrets?ns=ops", ""); rec.Code != http.StatusForbidden {
			t.Fatalf("scope 밖 secret 목록 = %d, want 403", rec.Code)
		}
	})

	t.Run("목록 조회 실패는 502", func(t *testing.T) {
		f, kube := manageClientFixture(t, adminScope(), objs...)
		kube.PrependReactor("list", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, stderrors.New("apiserver down")
		})
		if rec := f.manage(t, http.MethodGet, "/secrets", ""); rec.Code != http.StatusBadGateway {
			t.Fatalf("secret 목록 실패 = %d, want 502", rec.Code)
		}
	})
}

// TestSecretRestartRollsReferencingDeployments — Secret 재배포가 env·envFrom·volume·
// initContainer로 참조하는 Deployment만 rollout restart 한다.
func TestSecretRestartRollsReferencingDeployments(t *testing.T) {
	secretEnv := corev1.EnvVar{Name: "PW", ValueFrom: &corev1.EnvVarSource{
		SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "db"}, Key: "PASSWORD"},
	}}
	objs := []runtime.Object{
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "payments"}},
		deployment("payments", "envuser", int32p(1), corev1.PodSpec{
			Containers: []corev1.Container{{Name: "c", Env: []corev1.EnvVar{secretEnv}}},
		}),
		deployment("payments", "envfromuser", int32p(1), corev1.PodSpec{
			Containers: []corev1.Container{{Name: "c", EnvFrom: []corev1.EnvFromSource{
				{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "db"}}},
			}}},
		}),
		deployment("payments", "volumeuser", int32p(1), corev1.PodSpec{
			Containers: []corev1.Container{{Name: "c"}},
			Volumes: []corev1.Volume{{Name: "creds", VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{SecretName: "db"},
			}}},
		}),
		deployment("payments", "inituser", int32p(1), corev1.PodSpec{
			Containers:     []corev1.Container{{Name: "c"}},
			InitContainers: []corev1.Container{{Name: "init", Env: []corev1.EnvVar{secretEnv}}},
		}),
		deployment("payments", "bystander", int32p(1), corev1.PodSpec{Containers: []corev1.Container{{Name: "c"}}}),
	}

	t.Run("참조 워크로드가 모두 재배포된다", func(t *testing.T) {
		f, kube := manageClientFixture(t, adminScope(), objs...)
		rec := f.manage(t, http.MethodPost, "/secrets/payments/db/restart", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("secret restart = %d %s", rec.Code, rec.Body.String())
		}
		var res contract.ManagedActionResult
		if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil || !res.OK {
			t.Fatalf("restart result = %s", rec.Body.String())
		}
		sort.Strings(res.Affected)
		want := []string{"envfromuser", "envuser", "inituser", "volumeuser"}
		if strings.Join(res.Affected, ",") != strings.Join(want, ",") {
			t.Fatalf("affected = %v, want %v", res.Affected, want)
		}
		for _, name := range want {
			d, err := kube.AppsV1().Deployments("payments").Get(t.Context(), name, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if d.Spec.Template.Annotations["kubectl.kubernetes.io/restartedAt"] == "" {
				t.Fatalf("%s에 restartedAt annotation이 없습니다", name)
			}
		}
		if d, _ := kube.AppsV1().Deployments("payments").Get(t.Context(), "bystander", metav1.GetOptions{}); d.Spec.Template.Annotations["kubectl.kubernetes.io/restartedAt"] != "" {
			t.Fatal("참조하지 않는 Deployment까지 재배포되었습니다")
		}
	})

	t.Run("참조가 없으면 없다고 알린다", func(t *testing.T) {
		f, _ := manageClientFixture(t, adminScope(),
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "unused", Namespace: "payments"}})
		rec := f.manage(t, http.MethodPost, "/secrets/payments/unused/restart", "")
		var res contract.ManagedActionResult
		if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
			t.Fatal(err)
		}
		if len(res.Affected) != 0 || !strings.Contains(res.Message, "없습니다") {
			t.Fatalf("no-ref result = %+v", res)
		}
	})

	t.Run("참조 조회 실패는 502", func(t *testing.T) {
		f, kube := manageClientFixture(t, adminScope(), objs...)
		kube.PrependReactor("list", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, stderrors.New("apiserver down")
		})
		if rec := f.manage(t, http.MethodPost, "/secrets/payments/db/restart", ""); rec.Code != http.StatusBadGateway {
			t.Fatalf("참조 조회 실패 = %d, want 502", rec.Code)
		}
	})

	t.Run("재배포 실패는 중단하고 오류를 알린다", func(t *testing.T) {
		f, kube := manageClientFixture(t, adminScope(), objs...)
		kube.PrependReactor("patch", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, stderrors.New("boom")
		})
		if rec := f.manage(t, http.MethodPost, "/secrets/payments/db/restart", ""); rec.Code != http.StatusBadGateway {
			t.Fatalf("재배포 실패 = %d, want 502", rec.Code)
		}
	})
}

// TestSecretDetailListsReferencingPods — Secret 상세가 참조 Pod만 싣는다.
func TestSecretDetailListsReferencingPods(t *testing.T) {
	objs := []runtime.Object{
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "payments"},
			Type: corev1.SecretTypeOpaque, Data: map[string][]byte{"PASSWORD": []byte("s3cr3t")}},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "user-0", Namespace: "payments", UID: "uid-a"},
			Spec: corev1.PodSpec{Volumes: []corev1.Volume{{Name: "creds", VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{SecretName: "db"},
			}}}},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "loner", Namespace: "payments", UID: "uid-b"},
			Status:     corev1.PodStatus{Phase: corev1.PodRunning},
		},
	}
	f, _ := manageClientFixture(t, adminScope(), objs...)
	rec := f.manage(t, http.MethodGet, "/secrets/payments/db", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("secret detail = %d %s", rec.Code, rec.Body.String())
	}
	var out contract.ManagedSecretDetail
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Pods) != 1 || out.Pods[0].Name != "user-0" {
		t.Fatalf("참조 Pod 목록 = %+v", out.Pods)
	}
}
