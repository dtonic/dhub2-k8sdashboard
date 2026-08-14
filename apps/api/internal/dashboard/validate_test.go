package dashboard

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
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

func TestValidateSemanticBoundaries(t *testing.T) {
	refs := map[string]struct{}{"metrics.cpu.used": {}}
	validSeries := validDefinition()
	validSeries.Widgets[0] = Widget{ID: "trend", Title: "Trend", Type: "TimeSeries", Binding: "trends", QueryRefs: []string{"metrics.cpu.used"}, Layout: Layout{W: 3, H: 2}}
	validTable := validDefinition()
	validTable.Widgets[0] = Widget{ID: "table", Title: "Table", Type: "Table", Binding: "unhealthy", Options: &Options{MaxRows: 50}, Layout: Layout{W: 3, H: 2}}
	for name, def := range map[string]Definition{"series": validSeries, "table": validTable} {
		if err := Validate(def, refs); err != nil {
			t.Fatalf("%s rejected: %v", name, err)
		}
	}

	cases := map[string]func(*Definition){
		"schema":     func(d *Definition) { d.SchemaVersion = 2 },
		"identity":   func(d *Definition) { d.ID = "Invalid" },
		"limits":     func(d *Definition) { d.Widgets = nil },
		"variable":   func(d *Definition) { d.Variables = []Variable{{ID: "x", Label: "X", Kind: "other"}} },
		"widget":     func(d *Definition) { d.Widgets[0].Binding = "raw.query" },
		"empty refs": func(d *Definition) { d.Widgets[0].Type, d.Widgets[0].Binding = "TimeSeries", "trends" },
		"duplicate ref": func(d *Definition) {
			d.Widgets[0].Type, d.Widgets[0].Binding, d.Widgets[0].QueryRefs = "TimeSeries", "trends", []string{"metrics.cpu.used", "metrics.cpu.used"}
		},
		"unknown ref": func(d *Definition) {
			d.Widgets[0].Type, d.Widgets[0].Binding, d.Widgets[0].QueryRefs = "TimeSeries", "trends", []string{"metrics.unknown"}
		},
		"forbidden refs":    func(d *Definition) { d.Widgets[0].QueryRefs = []string{"metrics.cpu.used"} },
		"forbidden options": func(d *Definition) { d.Widgets[0].Options = &Options{MaxRows: 1} },
		"max rows": func(d *Definition) {
			d.Widgets[0].Type, d.Widgets[0].Binding, d.Widgets[0].Options = "Table", "unhealthy", &Options{MaxRows: 5001}
		},
		"bounds": func(d *Definition) { d.Widgets[0].Layout.X = 12 },
	}
	for name, mutate := range cases {
		d := validDefinition()
		mutate(&d)
		if err := Validate(d, refs); err == nil {
			t.Errorf("%s mutation was accepted", name)
		}
	}
}

func TestDecodeRejectsMalformedShapes(t *testing.T) {
	refs := map[string]struct{}{"metrics.cpu.used": {}}
	baseWidget := `{"id":"w","title":"W","type":"Stat","binding":"nodes.ready","layout":{"x":0,"y":0,"w":1,"h":1}}`
	cases := []string{
		`{`,
		`{"schemaVersion":1,"id":"x","title":"X","variables":{},"widgets":[]}`,
		`{"schemaVersion":1,"id":"x","title":"X","variables":[{"id":"x","label":"X"}],"widgets":[` + baseWidget + `]}`,
		`{"schemaVersion":1,"id":"x","title":"X","variables":[],"widgets":{}}`,
		`{"schemaVersion":1,"id":"x","title":"X","variables":[],"widgets":[{"id":"w","title":"W","type":1,"binding":"nodes.ready","layout":{"x":0,"y":0,"w":1,"h":1}}]}`,
		`{"schemaVersion":1,"id":"x","title":"X","variables":[],"widgets":[{"id":"w","title":"W","type":"TimeSeries","binding":"trends","layout":{"x":0,"y":0,"w":1,"h":1}}]}`,
		`{"schemaVersion":1,"id":"x","title":"X","variables":[],"widgets":[{"id":"w","title":"W","type":"Stat","binding":"nodes.ready","layout":1}]}`,
		`{"schemaVersion":1,"id":"x","title":"X","variables":[],"widgets":[{"id":"w","title":"W","type":"Table","binding":"unhealthy","options":1,"layout":{"x":0,"y":0,"w":1,"h":1}}]}`,
		`{"schemaVersion":1,"id":"x","title":"X","variables":[],"widgets":[{"id":"w","title":"W","type":"Table","binding":"unhealthy","options":{},"layout":{"x":0,"y":0,"w":1,"h":1}}]}`,
	}
	for _, raw := range cases {
		if _, err := DecodeAndValidate([]byte(raw), refs); err == nil {
			t.Errorf("malformed shape accepted: %s", raw)
		}
	}
	if _, err := DecodeAndValidate(bytes.Repeat([]byte(" "), MaxDefinitionBytes+1), refs); err == nil {
		t.Fatal("oversized input accepted")
	}
	typedMismatch := `{"schemaVersion":1,"id":"x","title":"X","variables":[{"id":1,"label":"X","kind":"scope"}],"widgets":[` + baseWidget + `]}`
	if _, err := DecodeAndValidate([]byte(typedMismatch), refs); err == nil {
		t.Fatal("typed field mismatch accepted")
	}
	for _, malformed := range []string{`{"x":`, `[1,`} {
		if err := ValidateJSONTokens([]byte(malformed)); err == nil {
			t.Errorf("malformed token stream accepted: %s", malformed)
		}
	}
}

func TestCursorRoundTripAndTamper(t *testing.T) {
	p := &Postgres{cursorKey: []byte("unit-test-cursor-key-0000000000000")}
	wantTime := time.Unix(0, 123456789).UTC()
	wantID := "11111111-1111-4111-8111-111111111111"
	cursor := p.encodeCursor(wantTime, wantID)
	gotTime, gotID, err := p.decodeCursor(cursor)
	if err != nil || !gotTime.Equal(wantTime) || gotID != wantID {
		t.Fatalf("round trip time=%v id=%s err=%v", gotTime, gotID, err)
	}
	for _, invalid := range []string{cursor + "x", strings.Repeat("x", 129), "***"} {
		if _, _, err := p.decodeCursor(invalid); !errors.Is(err, ErrInvalidCursor) {
			t.Errorf("invalid cursor %q: %v", invalid, err)
		}
	}
}

type scannerFunc func(...any) error

func (f scannerFunc) Scan(dest ...any) error { return f(dest...) }

func TestScanRejectsDriverAndStoredJSONErrors(t *testing.T) {
	driverErr := errors.New("driver failure")
	if _, err := scan(scannerFunc(func(...any) error { return driverErr })); !errors.Is(err, driverErr) {
		t.Fatalf("driver error=%v", err)
	}
	badJSON := scannerFunc(func(dest ...any) error {
		*(dest[0].(*string)) = "11111111-1111-4111-8111-111111111111"
		*(dest[1].(*string)) = "owner"
		*(dest[2].(*int64)) = 1
		*(dest[3].(*State)) = StateDraft
		*(dest[4].(*int)) = SchemaVersion
		*(dest[5].(*[]byte)) = []byte(`"invalid"`)
		*(dest[6].(*time.Time)) = time.Unix(1, 0)
		*(dest[7].(*time.Time)) = time.Unix(1, 0)
		return nil
	})
	if _, err := scan(badJSON); err == nil {
		t.Fatal("invalid stored definition JSON was accepted")
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
