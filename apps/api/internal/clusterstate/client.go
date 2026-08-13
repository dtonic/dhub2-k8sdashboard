package clusterstate

import (
	"fmt"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/metadata"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/flowcontrol"
)

// ProtobufAccept는 내장 타입에 대해 protobuf를 먼저 협상하고, 없으면 JSON으로 떨어집니다.
//
// **1순위 기준(클러스터 부하)에 직접 걸리는 설정입니다.** (ADR 0004 구현 규칙 6)
// Pod·Event처럼 객체 수가 많은 리소스를 통째로 watch하므로,
// API 서버의 직렬화 CPU와 전송 바이트가 JSON 대비 줄어듭니다.
const (
	ProtobufAccept      = "application/vnd.kubernetes.protobuf,application/json"
	ProtobufContentType = "application/vnd.kubernetes.protobuf"
)

// ClientOptions는 API 서버로 나가는 부하를 결정하는 값들입니다.
type ClientOptions struct {
	// Kubeconfig가 비어 있으면 in-cluster 설정을 씁니다.
	Kubeconfig string
	// QPS/Burst는 client-side rate limit입니다. informer는 최초 LIST 이후 요청이
	// 거의 없으므로 낮게 두어도 됩니다. 낮게 두는 편이 사고를 막습니다.
	QPS   float32
	Burst int
	// UserAgent는 API 서버 로그와 Priority & Fairness 분류에서 우리를 식별합니다.
	UserAgent string
	// DisableProtobuf는 protobuf를 지원하지 않는 aggregated API 뒤에서만 켭니다.
	DisableProtobuf bool
}

// RestConfig는 protobuf 협상까지 적용된 rest.Config를 만듭니다.
func RestConfig(o ClientOptions) (*rest.Config, error) {
	var (
		cfg *rest.Config
		err error
	)
	if o.Kubeconfig == "" {
		cfg, err = rest.InClusterConfig()
	} else {
		cfg, err = clientcmd.BuildConfigFromFlags("", o.Kubeconfig)
	}
	if err != nil {
		return nil, fmt.Errorf("kubernetes 접속 설정을 만들지 못했습니다: %w", err)
	}

	if !o.DisableProtobuf {
		cfg.AcceptContentTypes = ProtobufAccept
		cfg.ContentType = ProtobufContentType
	}
	if o.UserAgent != "" {
		cfg.UserAgent = o.UserAgent
	}
	if o.QPS > 0 {
		cfg.QPS = o.QPS
		cfg.Burst = o.Burst
		cfg.RateLimiter = flowcontrol.NewTokenBucketRateLimiter(o.QPS, o.Burst)
	}
	return cfg, nil
}

// Clients는 informer가 쓰는 두 종류의 클라이언트입니다.
//
// Metadata는 `PartialObjectMetadata`만 받는 클라이언트로, 소유 관계만 필요한
// 리소스(ReplicaSet)에 씁니다. spec/status를 아예 전송하지 않습니다. (ADR 0004 구현 규칙 5)
type Clients struct {
	Typed    kubernetes.Interface
	Metadata metadata.Interface
}

// NewClients는 rest.Config에서 두 클라이언트를 만듭니다.
func NewClients(cfg *rest.Config) (Clients, error) {
	typed, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return Clients{}, fmt.Errorf("typed 클라이언트 생성 실패: %w", err)
	}
	// metadata 클라이언트는 자체적으로 PartialObjectMetadata용 콘텐츠 협상을 설정하므로
	// 위에서 넣은 protobuf 설정을 덮어쓰지 않도록 복사본을 넘깁니다.
	metaCfg := rest.CopyConfig(cfg)
	metaCfg.AcceptContentTypes = ""
	metaCfg.ContentType = ""
	meta, err := metadata.NewForConfig(metaCfg)
	if err != nil {
		return Clients{}, fmt.Errorf("metadata 클라이언트 생성 실패: %w", err)
	}
	return Clients{Typed: typed, Metadata: meta}, nil
}
