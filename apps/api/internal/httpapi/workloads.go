package httpapi

// Deployment/Secret 관리 API (ADR 0014, #32)
// --------------------------------------------------------------------------
// 조회 경로(informer-only)와 격리된 관리 경로입니다. 요청 시점에 typed clientset을
// 직접 호출하고, platform.admin(또는 AUTH_MODE=none)만 허용하며, 모든 write는
// audit 로그를 남깁니다. Secret 값은 캐시에 상주시키지 않고 관리 응답으로만 흘립니다.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/contract"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/scope"
)

// manageTimeout은 관리용 API 호출 1건의 상한입니다.
const manageTimeout = 8 * time.Second

// requireManage는 관리 권한·클러스터 접근·clientset 가용성을 검사합니다.
// 실패 시 응답을 쓰고 (nil, false)를 반환합니다.
func (s *Server) requireManage(w http.ResponseWriter, r *http.Request, namespace string) (*manageCtx, bool) {
	sc := scope.From(r.Context())
	clusterID := r.PathValue("clusterId")
	cl, ok := sc.Cluster(clusterID)
	if !ok || !cl.Accessible() {
		writeError(w, r, http.StatusForbidden, "cluster_access_denied", "이 클러스터에 대한 권한이 없습니다.")
		return nil, false
	}
	if !sc.CanManageWorkloads {
		writeError(w, r, http.StatusForbidden, "forbidden", "워크로드 관리 권한이 없습니다.")
		return nil, false
	}
	if s.deps.KubeClient == nil {
		writeError(w, r, http.StatusServiceUnavailable, "manage_unavailable", "이 배포에서는 관리 기능을 사용할 수 없습니다.")
		return nil, false
	}
	// namespace가 지정된 요청은 그 namespace가 Scope 안에 있어야 합니다.
	if namespace != "" && !cl.AllowsNamespace(namespace) {
		writeError(w, r, http.StatusForbidden, "namespace_access_denied", "이 namespace에 대한 권한이 없습니다.")
		return nil, false
	}
	return &manageCtx{scope: sc, cluster: cl, clusterID: clusterID, subject: sc.Subject}, true
}

type manageCtx struct {
	scope     scope.Scope
	cluster   scope.Cluster
	clusterID string
	subject   string
}

// manageNamespaces는 목록 조회에서 쓸 namespace 집합입니다. 전체 접근이면 "".
func (m *manageCtx) listNamespace(requested string) (string, bool) {
	if requested != "" {
		return requested, m.cluster.AllowsNamespace(requested)
	}
	if m.cluster.All {
		return "", true // 전체 namespace
	}
	// 단일 namespace 스코프면 그 하나로.
	if len(m.cluster.Namespaces) == 1 {
		return m.cluster.Namespaces[0], true
	}
	return "", true // 여러 개면 전체 목록 후 필터
}

func (s *Server) auditManage(r *http.Request, action, kind, ns, name, subject string, err error) {
	outcome := "ok"
	if err != nil {
		outcome = "error"
	}
	s.deps.Logger.Info("manage-audit",
		"requestId", r.Header.Get(requestIDHeader),
		"action", action, "kind", kind, "namespace", ns, "name", name,
		"subject", subject, "outcome", outcome)
}

/* ── Deployment 목록 ──────────────────────────────────────────────────── */

func (s *Server) handleDeploymentList(w http.ResponseWriter, r *http.Request) {
	m, ok := s.requireManage(w, r, "")
	if !ok {
		return
	}
	ns, allowed := m.listNamespace(r.URL.Query().Get("ns"))
	if !allowed {
		writeError(w, r, http.StatusForbidden, "namespace_access_denied", "이 namespace에 대한 권한이 없습니다.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), manageTimeout)
	defer cancel()
	list, err := s.deps.KubeClient.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		writeError(w, r, http.StatusBadGateway, "list_failed", "Deployment 목록을 불러오지 못했습니다.")
		return
	}
	items := make([]contract.ManagedWorkload, 0, len(list.Items))
	for i := range list.Items {
		d := &list.Items[i]
		if !m.cluster.AllowsNamespace(d.Namespace) {
			continue
		}
		items = append(items, contract.ManagedWorkload{
			Namespace: d.Namespace, Name: d.Name, Kind: "Deployment",
			Ready: d.Status.ReadyReplicas, Desired: derefInt32(d.Spec.Replicas),
			UpdatedAt: d.CreationTimestamp.UTC().Format(time.RFC3339),
		})
	}
	sortWorkloads(items)
	writeJSON(w, http.StatusOK, contract.ManagedWorkloadListResponse{
		ClusterID: m.clusterID, GeneratedAt: s.nowRFC3339(), Items: items,
	})
}

/* ── Deployment 상세 ──────────────────────────────────────────────────── */

func (s *Server) handleDeploymentDetail(w http.ResponseWriter, r *http.Request) {
	ns, name := r.PathValue("namespace"), r.PathValue("name")
	m, ok := s.requireManage(w, r, ns)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), manageTimeout)
	defer cancel()
	d, err := s.deps.KubeClient.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		writeManageGetError(w, r, err, "Deployment")
		return
	}
	manifest, mErr := sanitizedManifest(d, "apps/v1", "Deployment")
	if mErr != nil {
		writeError(w, r, http.StatusInternalServerError, "manifest_error", "매니페스트를 직렬화하지 못했습니다.")
		return
	}
	pods := s.podsForSelector(ctx, ns, d.Spec.Selector)
	writeJSON(w, http.StatusOK, contract.ManagedDeploymentDetail{
		ClusterID: m.clusterID, Namespace: ns, Name: name, GeneratedAt: s.nowRFC3339(),
		Ready: d.Status.ReadyReplicas, Desired: derefInt32(d.Spec.Replicas),
		Manifest: manifest, Pods: pods,
	})
}

/* ── Deployment 수정 ──────────────────────────────────────────────────── */

func (s *Server) handleDeploymentUpdate(w http.ResponseWriter, r *http.Request) {
	ns, name := r.PathValue("namespace"), r.PathValue("name")
	m, ok := s.requireManage(w, r, ns)
	if !ok {
		return
	}
	var body struct {
		Manifest string `json:"manifest"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_body", "요청 본문이 올바르지 않습니다.")
		return
	}
	var desired appsv1.Deployment
	if err := json.Unmarshal([]byte(body.Manifest), &desired); err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_manifest", "매니페스트 JSON이 올바르지 않습니다.")
		return
	}
	if desired.Namespace != ns || desired.Name != name {
		writeError(w, r, http.StatusBadRequest, "manifest_mismatch", "매니페스트의 name/namespace가 경로와 다릅니다.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), manageTimeout)
	defer cancel()
	_, err := s.deps.KubeClient.AppsV1().Deployments(ns).Update(ctx, &desired, metav1.UpdateOptions{})
	s.auditManage(r, "update", "Deployment", ns, name, m.subject, err)
	if err != nil {
		writeManageWriteError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, contract.ManagedActionResult{OK: true, Message: "Deployment를 수정했습니다."})
}

/* ── Deployment 재배포 ────────────────────────────────────────────────── */

func (s *Server) handleDeploymentRestart(w http.ResponseWriter, r *http.Request) {
	ns, name := r.PathValue("namespace"), r.PathValue("name")
	m, ok := s.requireManage(w, r, ns)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), manageTimeout)
	defer cancel()
	err := s.restartDeployment(ctx, ns, name)
	s.auditManage(r, "restart", "Deployment", ns, name, m.subject, err)
	if err != nil {
		writeManageWriteError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, contract.ManagedActionResult{OK: true, Message: "재배포를 시작했습니다.", Affected: []string{name}})
}

// restartDeployment는 kubectl rollout restart와 같은 방식으로 template annotation을 갱신합니다.
func (s *Server) restartDeployment(ctx context.Context, ns, name string) error {
	patch := fmt.Sprintf(`{"spec":{"template":{"metadata":{"annotations":{"kubectl.kubernetes.io/restartedAt":%q}}}}}`,
		s.deps.Now().UTC().Format(time.RFC3339))
	_, err := s.deps.KubeClient.AppsV1().Deployments(ns).Patch(ctx, name, types.StrategicMergePatchType, []byte(patch), metav1.PatchOptions{})
	return err
}

/* ── Secret 목록 ──────────────────────────────────────────────────────── */

func (s *Server) handleSecretList(w http.ResponseWriter, r *http.Request) {
	m, ok := s.requireManage(w, r, "")
	if !ok {
		return
	}
	ns, allowed := m.listNamespace(r.URL.Query().Get("ns"))
	if !allowed {
		writeError(w, r, http.StatusForbidden, "namespace_access_denied", "이 namespace에 대한 권한이 없습니다.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), manageTimeout)
	defer cancel()
	list, err := s.deps.KubeClient.CoreV1().Secrets(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		writeError(w, r, http.StatusBadGateway, "list_failed", "Secret 목록을 불러오지 못했습니다.")
		return
	}
	items := make([]contract.ManagedWorkload, 0, len(list.Items))
	for i := range list.Items {
		sec := &list.Items[i]
		if !m.cluster.AllowsNamespace(sec.Namespace) {
			continue
		}
		// 목록은 메타만 — 값은 절대 싣지 않습니다. (ADR 0014)
		items = append(items, contract.ManagedWorkload{
			Namespace: sec.Namespace, Name: sec.Name, Kind: "Secret",
			SecretType: string(sec.Type),
			UpdatedAt:  sec.CreationTimestamp.UTC().Format(time.RFC3339),
		})
	}
	sortWorkloads(items)
	writeJSON(w, http.StatusOK, contract.ManagedWorkloadListResponse{
		ClusterID: m.clusterID, GeneratedAt: s.nowRFC3339(), Items: items,
	})
}

/* ── Secret 상세 (값 포함) ────────────────────────────────────────────── */

func (s *Server) handleSecretDetail(w http.ResponseWriter, r *http.Request) {
	ns, name := r.PathValue("namespace"), r.PathValue("name")
	m, ok := s.requireManage(w, r, ns)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), manageTimeout)
	defer cancel()
	sec, err := s.deps.KubeClient.CoreV1().Secrets(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		writeManageGetError(w, r, err, "Secret")
		return
	}
	// 값을 브라우저로 내보내는 것은 admin 전용·감사 대상입니다. (ADR 0014)
	s.auditManage(r, "read-values", "Secret", ns, name, m.subject, nil)
	data := make(map[string]string, len(sec.Data))
	for k, v := range sec.Data {
		data[k] = string(v) // 서버가 base64 디코딩한 평문
	}
	writeJSON(w, http.StatusOK, contract.ManagedSecretDetail{
		ClusterID: m.clusterID, Namespace: ns, Name: name, GeneratedAt: s.nowRFC3339(),
		SecretType: string(sec.Type), Data: data, Pods: s.podsReferencingSecret(ctx, ns, name),
	})
}

/* ── Secret 수정 ──────────────────────────────────────────────────────── */

func (s *Server) handleSecretUpdate(w http.ResponseWriter, r *http.Request) {
	ns, name := r.PathValue("namespace"), r.PathValue("name")
	m, ok := s.requireManage(w, r, ns)
	if !ok {
		return
	}
	var body struct {
		Data map[string]string `json:"data"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_body", "요청 본문이 올바르지 않습니다.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), manageTimeout)
	defer cancel()
	sec, err := s.deps.KubeClient.CoreV1().Secrets(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		writeManageGetError(w, r, err, "Secret")
		return
	}
	sec.Data = make(map[string][]byte, len(body.Data))
	for k, v := range body.Data {
		sec.Data[k] = []byte(v) // clientset이 다시 base64 인코딩합니다.
	}
	sec.StringData = nil
	_, err = s.deps.KubeClient.CoreV1().Secrets(ns).Update(ctx, sec, metav1.UpdateOptions{})
	s.auditManage(r, "update", "Secret", ns, name, m.subject, err)
	if err != nil {
		writeManageWriteError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, contract.ManagedActionResult{OK: true, Message: "Secret을 수정했습니다."})
}

/* ── Secret 재배포 (참조 워크로드 롤아웃) ─────────────────────────────── */

func (s *Server) handleSecretRestart(w http.ResponseWriter, r *http.Request) {
	ns, name := r.PathValue("namespace"), r.PathValue("name")
	m, ok := s.requireManage(w, r, ns)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), manageTimeout)
	defer cancel()
	// 이 Secret을 참조하는 Deployment를 모두 rollout restart 합니다.
	deps, err := s.deploymentsReferencingSecret(ctx, ns, name)
	if err != nil {
		writeError(w, r, http.StatusBadGateway, "list_failed", "참조 워크로드를 조회하지 못했습니다.")
		return
	}
	affected := make([]string, 0, len(deps))
	for _, dn := range deps {
		if rErr := s.restartDeployment(ctx, ns, dn); rErr != nil {
			s.auditManage(r, "restart", "Deployment", ns, dn, m.subject, rErr)
			writeManageWriteError(w, r, rErr)
			return
		}
		s.auditManage(r, "restart", "Deployment", ns, dn, m.subject, nil)
		affected = append(affected, dn)
	}
	msg := "재배포를 시작했습니다."
	if len(affected) == 0 {
		msg = "이 Secret을 참조하는 Deployment가 없습니다."
	}
	writeJSON(w, http.StatusOK, contract.ManagedActionResult{OK: true, Message: msg, Affected: affected})
}

/* ── 공통 헬퍼 ────────────────────────────────────────────────────────── */

func (s *Server) podsForSelector(ctx context.Context, ns string, sel *metav1.LabelSelector) []contract.ManagedPod {
	if sel == nil {
		return nil
	}
	ls := metav1.FormatLabelSelector(sel)
	list, err := s.deps.KubeClient.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: ls})
	if err != nil {
		return nil
	}
	return toManagedPods(list.Items)
}

func (s *Server) podsReferencingSecret(ctx context.Context, ns, secret string) []contract.ManagedPod {
	list, err := s.deps.KubeClient.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil
	}
	var out []corev1.Pod
	for i := range list.Items {
		if podReferencesSecret(&list.Items[i], secret) {
			out = append(out, list.Items[i])
		}
	}
	return toManagedPods(out)
}

func (s *Server) deploymentsReferencingSecret(ctx context.Context, ns, secret string) ([]string, error) {
	list, err := s.deps.KubeClient.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	var names []string
	for i := range list.Items {
		d := &list.Items[i]
		if podSpecReferencesSecret(&d.Spec.Template.Spec, secret) {
			names = append(names, d.Name)
		}
	}
	return names, nil
}

func podReferencesSecret(p *corev1.Pod, secret string) bool {
	return podSpecReferencesSecret(&p.Spec, secret)
}

// secretNamesFromPod은 pod가 참조하는 Secret 이름을 수집합니다(값 아님, 중복 제거). (#33)
func secretNamesFromPod(p *corev1.Pod) []string {
	seen := map[string]struct{}{}
	add := func(n string) {
		if n != "" {
			seen[n] = struct{}{}
		}
	}
	for _, v := range p.Spec.Volumes {
		if v.Secret != nil {
			add(v.Secret.SecretName)
		}
	}
	containers := append([]corev1.Container{}, p.Spec.Containers...)
	containers = append(containers, p.Spec.InitContainers...)
	for _, c := range containers {
		for _, ef := range c.EnvFrom {
			if ef.SecretRef != nil {
				add(ef.SecretRef.Name)
			}
		}
		for _, e := range c.Env {
			if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
				add(e.ValueFrom.SecretKeyRef.Name)
			}
		}
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func podSpecReferencesSecret(spec *corev1.PodSpec, secret string) bool {
	for _, v := range spec.Volumes {
		if v.Secret != nil && v.Secret.SecretName == secret {
			return true
		}
	}
	containers := append([]corev1.Container{}, spec.Containers...)
	containers = append(containers, spec.InitContainers...)
	for _, c := range containers {
		for _, ef := range c.EnvFrom {
			if ef.SecretRef != nil && ef.SecretRef.Name == secret {
				return true
			}
		}
		for _, e := range c.Env {
			if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil && e.ValueFrom.SecretKeyRef.Name == secret {
				return true
			}
		}
	}
	return false
}

func toManagedPods(pods []corev1.Pod) []contract.ManagedPod {
	out := make([]contract.ManagedPod, 0, len(pods))
	for i := range pods {
		p := &pods[i]
		ready := false
		var restarts int32
		for _, cs := range p.Status.ContainerStatuses {
			restarts += cs.RestartCount
			if cs.Ready {
				ready = true
			}
		}
		sev := contract.SeverityHealthy
		if p.Status.Phase != corev1.PodRunning && p.Status.Phase != corev1.PodSucceeded {
			sev = contract.SeverityWarning
		}
		out = append(out, contract.ManagedPod{
			Name: p.Name, UID: string(p.UID), Namespace: p.Namespace,
			Phase: string(p.Status.Phase), Ready: ready, Restarts: restarts, Severity: sev,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// sanitizedManifest는 서버 관리 필드·status를 제거한 관리자용 JSON을 만듭니다.
func sanitizedManifest(obj metav1.Object, apiVersion, kind string) (string, error) {
	obj.SetManagedFields(nil)
	obj.SetGeneration(0)
	obj.SetResourceVersion("")
	obj.SetUID("")
	obj.SetCreationTimestamp(metav1.Time{})
	ann := obj.GetAnnotations()
	delete(ann, "kubectl.kubernetes.io/last-applied-configuration")
	obj.SetAnnotations(ann)
	// status는 타입별로 비웁니다.
	if d, ok := obj.(*appsv1.Deployment); ok {
		d.Status = appsv1.DeploymentStatus{}
		d.TypeMeta = metav1.TypeMeta{APIVersion: apiVersion, Kind: kind}
	}
	raw, err := json.MarshalIndent(obj, "", "  ")
	return string(raw), err
}

func sortWorkloads(items []contract.ManagedWorkload) {
	sort.Slice(items, func(i, j int) bool {
		if items[i].Namespace != items[j].Namespace {
			return items[i].Namespace < items[j].Namespace
		}
		return items[i].Name < items[j].Name
	})
}

func derefInt32(p *int32) int32 {
	if p == nil {
		return 0
	}
	return *p
}

func writeManageGetError(w http.ResponseWriter, r *http.Request, err error, kind string) {
	if apierrors.IsNotFound(err) {
		writeError(w, r, http.StatusNotFound, "not_found", kind+"을(를) 찾을 수 없습니다.")
		return
	}
	if apierrors.IsForbidden(err) {
		writeError(w, r, http.StatusForbidden, "forbidden", "권한이 없습니다.")
		return
	}
	writeError(w, r, http.StatusBadGateway, "get_failed", kind+" 조회에 실패했습니다.")
}

func writeManageWriteError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case apierrors.IsNotFound(err):
		writeError(w, r, http.StatusNotFound, "not_found", "대상을 찾을 수 없습니다.")
	case apierrors.IsConflict(err):
		writeError(w, r, http.StatusConflict, "conflict", "다른 변경과 충돌했습니다. 다시 조회 후 시도하세요.")
	case apierrors.IsForbidden(err):
		writeError(w, r, http.StatusForbidden, "forbidden", "권한이 없습니다.")
	case apierrors.IsInvalid(err):
		writeError(w, r, http.StatusBadRequest, "invalid", "유효하지 않은 변경입니다.")
	default:
		var statusErr *apierrors.StatusError
		if errors.As(err, &statusErr) {
			writeError(w, r, http.StatusBadGateway, "write_failed", "변경을 적용하지 못했습니다.")
			return
		}
		writeError(w, r, http.StatusBadGateway, "write_failed", "변경을 적용하지 못했습니다.")
	}
}
