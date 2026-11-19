package config_test

import (
	"encoding/json"
	"os"
	"slices"
	"sort"
	"testing"
)

const schemaPath = "../../configs/schema.json"

type schemaNode struct {
	Type                 string                `json:"type"`
	AdditionalProperties *bool                 `json:"additionalProperties"`
	Required             []string              `json:"required"`
	Properties           map[string]schemaNode `json:"properties"`
}

// schemaLeaves walks object nodes and returns dotted leaf paths. It fails the
// test if any object allows extra keys or does not require all its keys,
// because the Go loader rejects unknown keys and requires every field.
func schemaLeaves(t *testing.T, n schemaNode, prefix string, out *[]string) {
	t.Helper()
	if n.Type != "object" {
		*out = append(*out, prefix)
		return
	}
	if n.AdditionalProperties == nil || *n.AdditionalProperties {
		t.Errorf("schema %q: additionalProperties must be false", prefix)
	}
	keys := make([]string, 0, len(n.Properties))
	for k := range n.Properties {
		keys = append(keys, k)
	}
	req := slices.Clone(n.Required)
	for _, k := range keys {
		full := k
		if prefix != "" {
			full = prefix + "." + k
		}
		if optionalKeys[full] {
			req = append(req, k) // optional in the loader and the schema
		}
	}
	sort.Strings(keys)
	sort.Strings(req)
	if !slices.Equal(keys, req) {
		t.Errorf("schema %q: required %v != properties %v", prefix, req, keys)
	}
	for _, k := range keys {
		p := k
		if prefix != "" {
			p = prefix + "." + k
		}
		schemaLeaves(t, n.Properties[k], p, out)
	}
}

func mapLeaves(m map[string]any, prefix string, out *[]string) {
	for k, v := range m {
		p := k
		if prefix != "" {
			p = prefix + "." + k
		}
		if child, ok := v.(map[string]any); ok {
			mapLeaves(child, p, out)
			continue
		}
		*out = append(*out, p)
	}
}

// optionalKeys mirrors OPTIONAL in scripts/gen_schema.py.
var optionalKeys = map[string]bool{"store.migrate_dsn_file": true}

// TestSchemaMatchesExample keeps configs/schema.json, configs/example.yaml and
// the loader (via TestLoadFileExample) describing the same set of keys.
func TestSchemaMatchesExample(t *testing.T) {
	t.Parallel()
	b, err := os.ReadFile(schemaPath)
	if err != nil {
		t.Fatalf("read schema: %v", err)
	}
	var root schemaNode
	if err := json.Unmarshal(b, &root); err != nil {
		t.Fatalf("parse schema: %v", err)
	}
	var fromSchema, fromExample []string
	schemaLeaves(t, root, "", &fromSchema)
	mapLeaves(exampleMap(t), "", &fromExample)
	sort.Strings(fromSchema)
	sort.Strings(fromExample)
	if !slices.Equal(fromSchema, fromExample) {
		t.Fatalf("schema leaves and example leaves differ:\nschema:  %v\nexample: %v", fromSchema, fromExample)
	}
}
