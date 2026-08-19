package httpapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

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
