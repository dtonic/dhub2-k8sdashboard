package resourcecatalog

// metadata 전용 Accept 협상의 단위 검증입니다.
//
// 핵심은 하나입니다 — metadata 협상이 실린 요청에서만 full-object 대안이 사라지고,
// 그 외 요청의 Accept는 **한 글자도** 바뀌지 않아야 합니다.

import (
	"net/http"
	"strings"
	"testing"
)

const (
	pomProtobuf = "application/vnd.kubernetes.protobuf;as=PartialObjectMetadataList;g=meta.k8s.io;v=v1"
	pomJSON     = "application/json;as=PartialObjectMetadataList;g=meta.k8s.io;v=v1"
	pomOneJSON  = "application/json;as=PartialObjectMetadata;g=meta.k8s.io;v=v1"
)

func TestStripFullObjectAcceptKeepsOnlyMetadataMediaTypes(t *testing.T) {
	cases := map[string]struct {
		in      string
		want    string
		changed bool
	}{
		"LIST 3종에서 평범한 json만 제거": {
			in:      pomProtobuf + "," + pomJSON + ",application/json",
			want:    pomProtobuf + "," + pomJSON,
			changed: true,
		},
		"단일 객체 GET도 같은 규칙": {
			in:      pomOneJSON + ",application/json",
			want:    pomOneJSON,
			changed: true,
		},
		"protobuf 대안도 제거": {
			in:      pomProtobuf + "," + pomJSON + ",application/vnd.kubernetes.protobuf,application/json",
			want:    pomProtobuf + "," + pomJSON,
			changed: true,
		},
		"이미 metadata뿐이면 그대로": {in: pomProtobuf + "," + pomJSON, want: pomProtobuf + "," + pomJSON},
		"typed 클라이언트 Accept는 그대로": {
			in:   "application/vnd.kubernetes.protobuf,application/json",
			want: "application/vnd.kubernetes.protobuf,application/json",
		},
		"discovery Accept는 그대로": {
			in:   "application/json;g=apidiscovery.k8s.io;v=v2;as=APIGroupDiscoveryList,application/json",
			want: "application/json;g=apidiscovery.k8s.io;v=v2;as=APIGroupDiscoveryList,application/json",
		},
		"평범한 json 하나는 그대로": {in: "application/json", want: "application/json"},
		"빈 값은 그대로":         {in: "", want: ""},
	}
	for name, tc := range cases {
		got, changed := stripFullObjectAccept(tc.in)
		if got != tc.want || changed != tc.changed {
			t.Errorf("%s: got=(%q,%v) want=(%q,%v)", name, got, changed, tc.want, tc.changed)
		}
		if strings.Contains(tc.in, partialObjectMetadataMarker) {
			for _, part := range strings.Split(got, ",") {
				if strings.TrimSpace(part) == "application/json" {
					t.Errorf("%s: full-object 대안이 남았습니다: %q", name, got)
				}
			}
		}
	}
}

type captureRoundTripper struct {
	accept string
	seen   int
}

func (c *captureRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	c.accept = req.Header.Get("Accept")
	c.seen++
	return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Header: make(http.Header), Request: req}, nil
}

func TestMetadataOnlyTransportClonesInsteadOfMutating(t *testing.T) {
	capture := &captureRoundTripper{}
	transport := &metadataOnlyTransport{base: capture}

	original := pomProtobuf + "," + pomJSON + ",application/json"
	req, err := http.NewRequest(http.MethodGet, "http://example.invalid/api/v1/namespaces/payments/secrets", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", original)
	if _, err := transport.RoundTrip(req); err != nil {
		t.Fatal(err)
	}

	if capture.accept != pomProtobuf+","+pomJSON {
		t.Fatalf("전송된 Accept=%q", capture.accept)
	}
	// 공유될 수 있는 원본 요청은 그대로여야 합니다.
	if req.Header.Get("Accept") != original {
		t.Fatalf("원본 요청 헤더가 변경되었습니다: %q", req.Header.Get("Accept"))
	}
}

func TestMetadataOnlyTransportPassesThroughNonMetadataRequests(t *testing.T) {
	capture := &captureRoundTripper{}
	transport := &metadataOnlyTransport{base: capture}

	const typed = "application/vnd.kubernetes.protobuf,application/json"
	req, err := http.NewRequest(http.MethodGet, "http://example.invalid/apis", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", typed)
	if _, err := transport.RoundTrip(req); err != nil {
		t.Fatal(err)
	}
	if capture.accept != typed {
		t.Fatalf("metadata가 아닌 Accept가 바뀌었습니다: %q", capture.accept)
	}
	if capture.seen != 1 {
		t.Fatalf("base transport 호출 %d회", capture.seen)
	}
}
