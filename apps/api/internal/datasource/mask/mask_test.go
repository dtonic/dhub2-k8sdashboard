package mask_test

import (
	"strings"
	"testing"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/datasource/mask"
)

func TestSecretsNeverSurviveMasking(t *testing.T) {
	cases := []struct {
		name   string
		in     string
		secret string
		kind   string
	}{
		{"Bearer 토큰", `auth header Bearer abcdef0123456789ghijk rejected`, "abcdef0123456789ghijk", "token"},
		{"JWT", `token=eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9`, "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9", "token"},
		{"password", `login failed password: hunter2secret for admin`, "hunter2secret", "password"},
		{"api key", `config reloaded api_key=sk-live-7f3ac91b22d4`, "sk-live-7f3ac91b22d4", "secret"},
		{"email", `user login succeeded for admin@example.com`, "admin@example.com", "email"},
		{"IP", `retrying upstream 10.42.0.17 attempt 3`, "10.42.0.17", "ip"},
		{"카드번호", `charge failed for 4111 1111 1111 1111`, "4111 1111 1111 1111", "card"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, spans := mask.Apply(c.in)
			if strings.Contains(out, c.secret) {
				t.Fatalf("원문이 그대로 남았습니다: %q", out)
			}
			if len(spans) == 0 {
				t.Fatalf("가려졌는데 span이 없습니다: %q", out)
			}
			if !strings.Contains(out, mask.Char) {
				t.Fatalf("가림 문자가 없습니다: %q", out)
			}
			found := false
			for _, s := range spans {
				if s.Kind == c.kind {
					found = true
				}
			}
			if !found {
				t.Errorf("kind=%s 인 span이 없습니다: %+v", c.kind, spans)
			}
		})
	}
}

func TestSpanOffsetsPointAtMaskedText(t *testing.T) {
	// UI는 offset을 그대로 써서 밑줄을 긋습니다. 가리기 **전** 기준이면 위치가 어긋납니다.
	in := "user admin@example.com from 10.0.0.1 logged in"
	out, spans := mask.Apply(in)
	if len(spans) != 2 {
		t.Fatalf("span 수=%d, want 2 (%+v)", len(spans), spans)
	}
	runes := []rune(out)
	for _, s := range spans {
		if s.Start < 0 || s.Start+s.Length > len(runes) {
			t.Fatalf("span이 범위를 벗어납니다: %+v (len=%d)", s, len(runes))
		}
		seg := string(runes[s.Start : s.Start+s.Length])
		if seg != strings.Repeat(mask.Char, s.Length) {
			t.Errorf("offset %d 위치가 가려진 구간이 아닙니다: %q", s.Start, seg)
		}
	}
}

func TestMaskedLengthDoesNotLeakOriginalLength(t *testing.T) {
	short, sp1 := mask.Apply("password: ab12cd34")
	long, sp2 := mask.Apply("password: ab12cd34ef56gh78ij90kl")
	if sp1[0].Length != sp2[0].Length {
		t.Fatalf("가림 길이가 원문 길이를 따라갑니다: %d vs %d", sp1[0].Length, sp2[0].Length)
	}
	if len(short) != len(long) {
		t.Errorf("본문 길이가 달라 원문 길이를 유추할 수 있습니다: %q / %q", short, long)
	}
}

func TestCleanMessageIsUntouched(t *testing.T) {
	in := "GET /api/v1/orders 200 in 34ms"
	out, spans := mask.Apply(in)
	if out != in {
		t.Errorf("멀쩡한 로그가 바뀌었습니다: %q", out)
	}
	if len(spans) != 0 {
		t.Errorf("span=%+v, want 없음", spans)
	}
	if spans == nil {
		t.Error("span은 nil이 아니라 빈 배열이어야 JSON에서 []로 나갑니다")
	}
}

func TestOverlappingRulesDoNotDoubleMask(t *testing.T) {
	// 이메일 안에 IP처럼 보이는 조각이 있어도 구간이 겹쳐 깨지면 안 됩니다.
	out, spans := mask.Apply("contact ops-1.2.3.4@example.com now")
	if strings.Contains(out, "example.com") {
		t.Errorf("이메일이 남았습니다: %q", out)
	}
	for i := 1; i < len(spans); i++ {
		prev := spans[i-1]
		if spans[i].Start < prev.Start+prev.Length {
			t.Fatalf("span이 겹칩니다: %+v", spans)
		}
	}
}
