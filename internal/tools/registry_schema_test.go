package tools

import (
	"testing"
)

// Provider APIs (Gemini especially) reject tool schemas whose properties have
// no "type", or arrays without "items". Catch that for every registered tool
// at test time instead of as a runtime 400 from a provider.
func TestToolSchemasAreStrictValidatorSafe(t *testing.T) {
	r := NewRegistry(nil, nil)
	for _, d := range r.Definitions() {
		props := d.Parameters
		// grep uses the full-schema form {"type":"object","properties":{...}}
		if typ, _ := props["type"].(string); typ == "object" {
			if p, ok := props["properties"].(map[string]any); ok {
				props = p
			}
		}
		lintSchemaProps(t, d.Name, props, "")
		for _, req := range d.Required {
			if _, ok := props[req]; !ok {
				t.Errorf("%s: required parameter %q is not declared", d.Name, req)
			}
		}
	}
}

func lintSchemaProps(t *testing.T, tool string, props map[string]any, path string) {
	t.Helper()
	for name, v := range props {
		m, ok := v.(map[string]any)
		if !ok {
			t.Errorf("%s: %s%s is not a schema object", tool, path, name)
			continue
		}
		typ, _ := m["type"].(string)
		if typ == "" {
			t.Errorf("%s: %s%s has no \"type\"", tool, path, name)
		}
		if typ != "array" {
			continue
		}
		items, ok := m["items"].(map[string]any)
		if !ok {
			t.Errorf("%s: array %s%s has no \"items\"", tool, path, name)
			continue
		}
		it, _ := items["type"].(string)
		if it == "" {
			t.Errorf("%s: items of %s%s have no \"type\"", tool, path, name)
		}
		if it == "object" {
			if p, ok := items["properties"].(map[string]any); ok {
				lintSchemaProps(t, tool, p, path+name+"[].")
			}
		}
	}
}
