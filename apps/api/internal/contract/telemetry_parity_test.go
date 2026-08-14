package contract

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// 이 파일은 Go 타입이 정본 스키마(packages/contracts/schema/telemetry.schema.json)와
// **속성 이름·필수 여부**가 같음을 reflection으로 증명합니다. (이슈 #4)
// TS 쪽 동등성은 packages/contracts의 tsc 검사와 node --test가 증명합니다.

const schemaPath = "../../../../packages/contracts/schema/telemetry.schema.json"

type schemaDef struct {
	Properties map[string]json.RawMessage `json:"properties"`
	Required   []string                   `json:"required"`
	Enum       []string                   `json:"enum"`
	// Labels 한도 검증용. additionalProperties는 다른 $def에서 bool(false)이므로 raw로 받습니다.
	MaxProperties        *int                     `json:"maxProperties"`
	PropertyNames        *struct{ MaxLength int } `json:"propertyNames"`
	AdditionalProperties json.RawMessage          `json:"additionalProperties"`
}

func loadSchema(t *testing.T) map[string]schemaDef {
	t.Helper()
	raw, err := os.ReadFile(filepath.FromSlash(schemaPath))
	if err != nil {
		t.Fatalf("정본 스키마를 읽지 못했습니다: %v", err)
	}
	var doc struct {
		Defs map[string]schemaDef `json:"$defs"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("스키마 파싱 실패: %v", err)
	}
	return doc.Defs
}

// jsonFields는 구조체의 json 태그를 (이름 → required 여부)로 폅니다.
// omitempty가 없으면 required로 봅니다 — 항상 직렬화되는 필드가 스키마의 required입니다.
func jsonFields(t *testing.T, typ reflect.Type) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for i := 0; i < typ.NumField(); i++ {
		tag := typ.Field(i).Tag.Get("json")
		if tag == "" || tag == "-" {
			t.Fatalf("%s.%s에 json 태그가 없습니다", typ.Name(), typ.Field(i).Name)
		}
		parts := strings.Split(tag, ",")
		required := true
		for _, p := range parts[1:] {
			if p == "omitempty" {
				required = false
			}
		}
		out[parts[0]] = required
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func TestGoTypesMatchCanonicalSchema(t *testing.T) {
	defs := loadSchema(t)
	cases := []struct {
		def string
		typ reflect.Type
	}{
		{"EntityRef", reflect.TypeOf(EntityRef{})},
		{"TelemetryScope", reflect.TypeOf(TelemetryScope{})},
		{"MetricRecord", reflect.TypeOf(MetricRecord{})},
		{"LogRecord", reflect.TypeOf(LogRecord{})},
		{"EventRecord", reflect.TypeOf(EventRecord{})},
		{"AlertRecord", reflect.TypeOf(AlertRecord{})},
	}
	for _, c := range cases {
		def, ok := defs[c.def]
		if !ok {
			t.Fatalf("스키마 $defs에 %s가 없습니다", c.def)
		}
		fields := jsonFields(t, c.typ)

		// 속성 이름 집합이 정확히 같아야 합니다.
		if got, want := sortedKeys(fields), sortedKeys(def.Properties); !reflect.DeepEqual(got, want) {
			t.Errorf("%s 속성 이름 불일치\n  Go:     %v\n  schema: %v", c.def, got, want)
		}
		// 필수 여부: omitempty 없는 필드 집합 == schema.required
		var goRequired []string
		for name, req := range fields {
			if req {
				goRequired = append(goRequired, name)
			}
		}
		sort.Strings(goRequired)
		wantRequired := append([]string(nil), def.Required...)
		sort.Strings(wantRequired)
		if !reflect.DeepEqual(goRequired, wantRequired) {
			t.Errorf("%s 필수 필드 불일치\n  Go:     %v\n  schema: %v", c.def, goRequired, wantRequired)
		}
	}
}

func TestMetricUnitsMatchSchemaEnum(t *testing.T) {
	defs := loadSchema(t)
	var got []string
	for _, u := range MetricUnits {
		got = append(got, string(u))
	}
	if !reflect.DeepEqual(got, defs["MetricUnit"].Enum) {
		t.Errorf("MetricUnit 불일치\n  Go:     %v\n  schema: %v", got, defs["MetricUnit"].Enum)
	}
}

func TestReservedLabelKeysAndLimitsMatchSchema(t *testing.T) {
	defs := loadSchema(t)
	labels := defs["Labels"]

	want := sortedKeys(labels.Properties)
	got := append([]string(nil), ReservedLabelKeys...)
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("예약 라벨 키 불일치\n  Go:     %v\n  schema: %v", got, want)
	}
	for key, sub := range labels.Properties {
		if string(sub) != "false" {
			t.Errorf("예약 키 %s는 스키마에서 false여야 합니다: %s", key, sub)
		}
	}
	if labels.MaxProperties == nil || *labels.MaxProperties != MaxTelemetryLabels {
		t.Errorf("라벨 수 상한 불일치: Go=%d schema=%v", MaxTelemetryLabels, labels.MaxProperties)
	}
	if labels.PropertyNames == nil || labels.PropertyNames.MaxLength != MaxTelemetryLabelKeyLen {
		t.Errorf("라벨 키 길이 상한 불일치: Go=%d schema=%v", MaxTelemetryLabelKeyLen, labels.PropertyNames)
	}
	var valueSchema struct{ MaxLength int }
	if err := json.Unmarshal(labels.AdditionalProperties, &valueSchema); err != nil || valueSchema.MaxLength != MaxTelemetryLabelValueLen {
		t.Errorf("라벨 값 길이 상한 불일치: Go=%d schema=%s (err=%v)", MaxTelemetryLabelValueLen, labels.AdditionalProperties, err)
	}
}
