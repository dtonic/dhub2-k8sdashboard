package dashboard

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestSharedRuntimeParityCorpus(t *testing.T) {
	var corpus struct {
		QueryRefs []string `json:"queryRefs"`
		Cases     []struct {
			Name  string `json:"name"`
			Valid bool   `json:"valid"`
			Raw   string `json:"raw"`
		} `json:"cases"`
	}
	b, err := os.ReadFile("../../../../packages/dashboard-schema/test/fixtures/dashboard-parity.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &corpus); err != nil {
		t.Fatal(err)
	}
	refs := make(map[string]struct{}, len(corpus.QueryRefs))
	for _, ref := range corpus.QueryRefs {
		refs[ref] = struct{}{}
	}
	for _, tc := range corpus.Cases {
		_, err := DecodeAndValidate([]byte(tc.Raw), refs)
		if (err == nil) != tc.Valid {
			t.Errorf("%s valid=%v err=%v", tc.Name, tc.Valid, err)
		}
	}
}

func validDefinition() Definition {
	return Definition{SchemaVersion: 1, ID: "custom", Title: "Custom", Variables: []Variable{}, Widgets: []Widget{{ID: "ready", Title: "Ready", Type: "Stat", Binding: "nodes.ready", Layout: Layout{X: 0, Y: 0, W: 3, H: 2}}}}
}

func TestValidateClosedDSL(t *testing.T) {
	refs := map[string]struct{}{"metrics.cpu.used": {}}
	if err := Validate(validDefinition(), refs); err != nil {
		t.Fatal(err)
	}
	cases := []string{
		`{"schemaVersion":1,"schemaVersion":1,"id":"custom","title":"x","variables":[],"widgets":[]}`,
		`{"schemaVersion":1,"id":"custom","title":"x","variables":[],"widgets":[{"id":"w","title":"x","type":"Stat","binding":"nodes.ready","expr":"up","layout":{"x":0,"y":0,"w":1,"h":1}}]}`,
		`{"schemaVersion":1,"id":"custom","title":"x","variables":[],"widgets":[{"id":"w","title":"x","type":"TimeSeries","binding":"trends","queryRefs":["raw.query"],"layout":{"x":0,"y":0,"w":1,"h":1}}]}`,
		`{"schemaVersion":1,"id":"custom","title":"x","description":null,"variables":[],"widgets":[{"id":"w","title":"x","type":"Stat","binding":"nodes.ready","layout":{"x":0,"y":0,"w":1,"h":1}}]}`,
		`{"schemaVersion":1,"id":"custom","title":"x","description":"","variables":[],"widgets":[{"id":"w","title":"x","type":"Stat","binding":"nodes.ready","layout":{"x":0,"y":0,"w":1,"h":1}}]}`,
		`{"schemaVersion":1,"id":"custom","title":"x","variables":[],"widgets":[{"id":"w","title":"x","type":"Stat","binding":"nodes.ready","queryRefs":null,"layout":{"x":0,"y":0,"w":1,"h":1}}]}`,
		`{"schemaVersion":1,"id":"custom","title":"x","variables":[],"widgets":[{"id":"w","title":"x","type":"Table","binding":"unhealthy","options":null,"layout":{"x":0,"y":0,"w":1,"h":1}}]}`,
		`{"schemaVersion":1,"id":"custom","title":"x","variables":[],"widgets":[{"id":"w","title":"x","type":"Stat","binding":"nodes.ready","layout":{"x":0,"y":0,"w":1,"h":1}}]} garbage`,
	}
	for _, raw := range cases {
		if _, err := DecodeAndValidate([]byte(raw), refs); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}

func TestLayoutAndCodePointLimits(t *testing.T) {
	d := validDefinition()
	d.Title = strings.Repeat("한", 160)
	if err := Validate(d, nil); err != nil {
		t.Fatal(err)
	}
	d.Title += "한"
	if err := Validate(d, nil); err == nil {
		t.Fatal("accepted 161 code points")
	}
	d = validDefinition()
	d.Widgets = append(d.Widgets, Widget{ID: "overlap", Title: "x", Type: "Gauge", Binding: "pods.runningPercent", Layout: Layout{X: 2, Y: 1, W: 2, H: 2}})
	if err := Validate(d, nil); err == nil {
		t.Fatal("accepted overlap")
	}
}

func TestCanonicalStableAndDoesNotEscapeHTML(t *testing.T) {
	a, sha1, err := Canonical(validDefinition())
	if err != nil {
		t.Fatal(err)
	}
	b, sha2, _ := Canonical(validDefinition())
	if !bytes.Equal(a, b) || sha1 != sha2 {
		t.Fatal("canonical output changed")
	}
	d := validDefinition()
	d.Description = "a < b"
	out, _, _ := Canonical(d)
	if bytes.Contains(out, []byte(`\u003c`)) || out[len(out)-1] != '\n' {
		t.Fatalf("bad canonical: %s", out)
	}
}

func TestCanonicalFileSizeBoundary(t *testing.T) {
	d := validDefinition()
	b, err := canonicalBytes(d)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateCanonicalSize(d, len(b)); err != nil {
		t.Fatalf("exact boundary rejected: %v", err)
	}
	if err := validateCanonicalSize(d, len(b)-1); err == nil {
		t.Fatal("over-bound canonical definition accepted")
	}
}

func BenchmarkValidate24Widgets(b *testing.B) {
	d := validDefinition()
	d.Widgets = nil
	for i := 0; i < 24; i++ {
		d.Widgets = append(d.Widgets, Widget{ID: "w" + string(rune('a'+i)), Title: "x", Type: "Stat", Binding: "nodes.ready", Layout: Layout{X: (i % 6) * 2, Y: (i / 6) * 2, W: 2, H: 2}})
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := Validate(d, nil); err != nil {
			b.Fatal(err)
		}
	}
}
