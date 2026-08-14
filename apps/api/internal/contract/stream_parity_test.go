package contract

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

// Go 스트림 타입이 정본 스키마(packages/contracts/schema/stream.schema.json)와
// 속성 이름·필수 여부·enum이 같음을 reflection으로 증명합니다. (#12)
// TS 쪽 동등성은 packages/contracts/test/stream-parity.test.mjs가 증명합니다.

const streamSchemaPath = "../../../../packages/contracts/schema/stream.schema.json"

func loadStreamSchema(t *testing.T) map[string]schemaDef {
	t.Helper()
	raw, err := os.ReadFile(filepath.FromSlash(streamSchemaPath))
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

func TestEventEnvelopeMatchesCanonicalSchema(t *testing.T) {
	defs := loadStreamSchema(t)
	def, ok := defs["EventEnvelope"]
	if !ok {
		t.Fatal("스키마 $defs에 EventEnvelope가 없습니다")
	}
	fields := jsonFields(t, reflect.TypeOf(EventEnvelope{}))

	if got, want := sortedKeys(fields), sortedKeys(def.Properties); !reflect.DeepEqual(got, want) {
		t.Errorf("EventEnvelope 속성 이름 불일치\n  Go:     %v\n  schema: %v", got, want)
	}
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
		t.Errorf("EventEnvelope 필수 필드 불일치\n  Go:     %v\n  schema: %v", goRequired, wantRequired)
	}
}

func TestStreamEnumsMatchSchema(t *testing.T) {
	defs := loadStreamSchema(t)

	var kinds []string
	for _, k := range StreamEventKinds {
		kinds = append(kinds, string(k))
	}
	if !reflect.DeepEqual(kinds, defs["StreamEventKind"].Enum) {
		t.Errorf("StreamEventKind 불일치\n  Go:     %v\n  schema: %v", kinds, defs["StreamEventKind"].Enum)
	}

	var actions []string
	for _, a := range StreamEventActions {
		actions = append(actions, string(a))
	}
	if !reflect.DeepEqual(actions, defs["StreamEventAction"].Enum) {
		t.Errorf("StreamEventAction 불일치\n  Go:     %v\n  schema: %v", actions, defs["StreamEventAction"].Enum)
	}
}

func TestStreamIDLimitMatchesSchema(t *testing.T) {
	defs := loadStreamSchema(t)
	var idSchema struct{ MaxLength int }
	raw := defs["EventEnvelope"].Properties["id"]
	if err := json.Unmarshal(raw, &idSchema); err != nil || idSchema.MaxLength != MaxStreamEventIDLen {
		t.Errorf("id 길이 상한 불일치: Go=%d schema=%s (err=%v)", MaxStreamEventIDLen, raw, err)
	}
}
