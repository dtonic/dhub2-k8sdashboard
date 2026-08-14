package dashboard

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
)

var idPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)

func SHA256(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

func DecodeAndValidate(raw []byte, allowedRefs map[string]struct{}) (Definition, error) {
	if len(raw) > MaxDefinitionBytes {
		return Definition{}, fmt.Errorf("definition exceeds %d bytes", MaxDefinitionBytes)
	}
	if err := rejectDuplicateKeys(raw); err != nil {
		return Definition{}, err
	}
	if err := validateShape(raw); err != nil {
		return Definition{}, err
	}
	var def Definition
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&def); err != nil {
		return Definition{}, fmt.Errorf("invalid definition: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return Definition{}, fmt.Errorf("invalid trailing JSON")
	}
	if err := Validate(def, allowedRefs); err != nil {
		return Definition{}, err
	}
	return def, nil
}

func validateShape(raw []byte) error {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil {
		return err
	}
	if err := exactRaw(root, []string{"schemaVersion", "id", "title", "description", "variables", "widgets"}, []string{"schemaVersion", "id", "title", "variables", "widgets"}); err != nil {
		return err
	}
	if v, ok := root["description"]; ok {
		var s string
		if bytes.Equal(bytes.TrimSpace(v), []byte("null")) || json.Unmarshal(v, &s) != nil || len([]rune(s)) == 0 {
			return fmt.Errorf("description must be a string")
		}
	}
	var variables []map[string]json.RawMessage
	if err := json.Unmarshal(root["variables"], &variables); err != nil {
		return fmt.Errorf("variables must be an array")
	}
	for _, v := range variables {
		if err := exactRaw(v, []string{"id", "label", "kind"}, []string{"id", "label", "kind"}); err != nil {
			return err
		}
	}
	var widgets []map[string]json.RawMessage
	if err := json.Unmarshal(root["widgets"], &widgets); err != nil {
		return fmt.Errorf("widgets must be an array")
	}
	for _, w := range widgets {
		var typ string
		if json.Unmarshal(w["type"], &typ) != nil {
			return fmt.Errorf("widget type must be a string")
		}
		allowed := []string{"id", "title", "type", "binding", "layout"}
		required := append([]string(nil), allowed...)
		if typ == "TimeSeries" {
			allowed = append(allowed, "queryRefs")
			required = append(required, "queryRefs")
		}
		if typ == "Table" || typ == "EventTimeline" {
			allowed = append(allowed, "options")
		}
		if err := exactRaw(w, allowed, required); err != nil {
			return err
		}
		var layout map[string]json.RawMessage
		if json.Unmarshal(w["layout"], &layout) != nil {
			return fmt.Errorf("layout must be an object")
		}
		if err := exactRaw(layout, []string{"x", "y", "w", "h"}, []string{"x", "y", "w", "h"}); err != nil {
			return err
		}
		if o, ok := w["options"]; ok {
			var options map[string]json.RawMessage
			if json.Unmarshal(o, &options) != nil {
				return fmt.Errorf("options must be an object")
			}
			if err := exactRaw(options, []string{"maxRows"}, []string{"maxRows"}); err != nil {
				return err
			}
		}
	}
	return nil
}
func exactRaw(m map[string]json.RawMessage, allowed, required []string) error {
	a := map[string]bool{}
	for _, k := range allowed {
		a[k] = true
	}
	for k := range m {
		if !a[k] {
			return fmt.Errorf("unknown property %s", k)
		}
	}
	for _, k := range required {
		v, ok := m[k]
		if !ok || bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			return fmt.Errorf("property %s is required", k)
		}
	}
	return nil
}

func rejectDuplicateKeys(raw []byte) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	var walk func() error
	walk = func() error {
		t, err := dec.Token()
		if err != nil {
			return err
		}
		d, ok := t.(json.Delim)
		if !ok {
			return nil
		}
		switch d {
		case '{':
			seen := map[string]bool{}
			for dec.More() {
				k, err := dec.Token()
				if err != nil {
					return err
				}
				key := k.(string)
				if seen[key] {
					return fmt.Errorf("duplicate JSON key %q", key)
				}
				seen[key] = true
				if err := walk(); err != nil {
					return err
				}
			}
			_, err = dec.Token()
			return err
		case '[':
			for dec.More() {
				if err := walk(); err != nil {
					return err
				}
			}
			_, err = dec.Token()
			return err
		}
		return nil
	}
	return walk()
}

func ValidateJSONTokens(raw []byte) error { return rejectDuplicateKeys(raw) }

func Validate(d Definition, refs map[string]struct{}) error {
	if d.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported schemaVersion")
	}
	if !validText(d.ID) || !idPattern.MatchString(d.ID) || !validText(d.Title) || len([]rune(d.Description)) > 160 {
		return fmt.Errorf("invalid dashboard identity")
	}
	if len(d.Variables) > 2 || len(d.Widgets) < 1 || len(d.Widgets) > 24 {
		return fmt.Errorf("dashboard limits exceeded")
	}
	ids, kinds := map[string]bool{}, map[string]bool{}
	for _, v := range d.Variables {
		if !validText(v.ID) || !idPattern.MatchString(v.ID) || ids[v.ID] || !validText(v.Label) || (v.Kind != "scope" && v.Kind != "range") || kinds[v.Kind] {
			return fmt.Errorf("invalid variable")
		}
		ids[v.ID], kinds[v.Kind] = true, true
	}
	ids = map[string]bool{}
	occupied := [96][12]bool{}
	bindings := map[string]map[string]bool{
		"TimeSeries": {"trends": true}, "Stat": {"nodes.ready": true, "pods.running": true}, "Gauge": {"pods.runningPercent": true},
		"Table": {"unhealthy": true}, "LogStream": {"unsupported.logs": true}, "EventTimeline": {"events": true},
	}
	for _, w := range d.Widgets {
		if !validText(w.ID) || !idPattern.MatchString(w.ID) || ids[w.ID] || !validText(w.Title) || !bindings[w.Type][w.Binding] {
			return fmt.Errorf("invalid widget")
		}
		ids[w.ID] = true
		if w.Type == "TimeSeries" {
			if len(w.QueryRefs) < 1 || len(w.QueryRefs) > 8 {
				return fmt.Errorf("invalid queryRefs")
			}
			seen := map[string]bool{}
			for _, r := range w.QueryRefs {
				if seen[r] {
					return fmt.Errorf("duplicate queryRef")
				}
				if _, ok := refs[r]; !ok {
					return fmt.Errorf("unknown queryRef")
				}
				seen[r] = true
			}
		} else if len(w.QueryRefs) > 0 {
			return fmt.Errorf("queryRefs forbidden for widget type")
		}
		if (w.Type == "Table" || w.Type == "EventTimeline") != (w.Options != nil) && w.Options != nil {
			return fmt.Errorf("options forbidden for widget type")
		}
		if w.Options != nil && (w.Options.MaxRows < 1 || w.Options.MaxRows > 5000) {
			return fmt.Errorf("invalid maxRows")
		}
		l := w.Layout
		if l.X < 0 || l.Y < 0 || l.W < 1 || l.H < 1 || l.X+l.W > 12 || l.Y+l.H > 96 {
			return fmt.Errorf("layout out of bounds")
		}
		for y := l.Y; y < l.Y+l.H; y++ {
			for x := l.X; x < l.X+l.W; x++ {
				if occupied[y][x] {
					return fmt.Errorf("layout overlap")
				}
				occupied[y][x] = true
			}
		}
	}
	return validateCanonicalSize(d, MaxDefinitionBytes)
}

func validText(s string) bool { n := len([]rune(s)); return n > 0 && n <= 160 }
