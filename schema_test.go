package systempackage

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

func TestSchemaCopyAndWireFields(t *testing.T) {
	raw := Schema()
	want := append([]byte(nil), raw...)
	raw[0] = '!'
	if !bytes.Equal(Schema(), want) {
		t.Fatal("caller changed the embedded schema")
	}
	var schema map[string]any
	if err := json.Unmarshal(want, &schema); err != nil {
		t.Fatal(err)
	}
	checkWireSchema(t, reflect.TypeOf(Recipe{}), schema)
	if RecipeMediaType != "application/vnd.kappliance.node-installer.recipe.v1+json" || RawMediaType != "application/octet-stream" {
		t.Fatal("v1 media types changed")
	}
}

// Check every nested wire field, including optional resources and map values.
func checkWireSchema(t *testing.T, typ reflect.Type, schema map[string]any) {
	t.Helper()
	if typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	switch typ.Kind() {
	case reflect.Struct:
		properties, ok := schema["properties"].(map[string]any)
		if !ok || schema["additionalProperties"] != false || len(properties) != typ.NumField() {
			t.Fatalf("schema does not match %s", typ)
		}
		for i := range typ.NumField() {
			field := typ.Field(i)
			tag := strings.Split(field.Tag.Get("json"), ",")
			name := tag[0]
			if len(tag) > 1 && tag[1] == "omitempty" {
				for _, required := range schema["required"].([]any) {
					if required == name {
						t.Fatalf("schema requires optional wire field %s.%s", typ, name)
					}
				}
			}
			child, ok := properties[name].(map[string]any)
			if !ok {
				t.Fatalf("schema is missing %s.%s", typ, name)
			}
			checkWireSchema(t, field.Type, child)
		}
	case reflect.Slice:
		child, ok := schema["items"].(map[string]any)
		if !ok {
			t.Fatalf("schema is missing items for %s", typ)
		}
		checkWireSchema(t, typ.Elem(), child)
	case reflect.Map:
		child, ok := schema["additionalProperties"].(map[string]any)
		if !ok {
			t.Fatalf("schema is missing map values for %s", typ)
		}
		checkWireSchema(t, typ.Elem(), child)
	default:
		want := map[reflect.Kind]string{reflect.String: "string", reflect.Bool: "boolean", reflect.Int: "integer"}[typ.Kind()]
		if got, ok := schema["type"]; ok && got != want {
			t.Fatalf("schema type for %s: %v, want %s", typ, got, want)
		}
	}
}

func TestSchemaGenericPlatformAndRuntime(t *testing.T) {
	var schema map[string]any
	if err := json.Unmarshal(Schema(), &schema); err != nil {
		t.Fatal(err)
	}
	properties := schema["properties"].(map[string]any)
	platform := properties["platforms"].(map[string]any)["items"].(map[string]any)["properties"].(map[string]any)
	for key, value := range map[string]string{"os": "freebsd", "architecture": "arm64", "adapter": "custom-adapter", "containerdVersion": "9.12.34-rc.1+vendor.2", "shimProfile": "7.0-next"} {
		field := platform[key].(map[string]any)
		if field["const"] != nil || field["enum"] != nil {
			t.Fatal("schema contains platform qualification", key)
		}
		pattern := regexp.MustCompile(field["pattern"].(string))
		if !pattern.MatchString(value) || pattern.MatchString("../host") || pattern.MatchString(strings.Repeat("a", 129)) {
			t.Fatal("schema platform identifier bounds differ", key)
		}
	}
	containerdSchema := properties["containerd"].(map[string]any)
	for _, field := range containerdSchema["required"].([]any) {
		if field == "environment" {
			t.Fatal("schema requires the optional generic environment")
		}
	}
	containerd := containerdSchema["properties"].(map[string]any)
	for _, key := range []string{"handler", "snapshotter"} {
		field := containerd[key].(map[string]any)
		if field["const"] != nil || field["enum"] != nil || !regexp.MustCompile(field["pattern"].(string)).MatchString("custom-runtime") {
			t.Fatal("schema contains runtime profile qualification", key)
		}
	}
	env := containerd["environment"].(map[string]any)
	if env["properties"] != nil || env["additionalProperties"] == false || env["required"] != nil {
		t.Fatal("schema requires application-specific environment variables")
	}
	if schema["allOf"] != nil {
		t.Fatal("schema contains a conditional application profile")
	}
	dependencies := schema["dependentRequired"].(map[string]any)
	if !reflect.DeepEqual(dependencies, map[string]any{"pause": []any{"containerd"}}) {
		t.Fatal("pause dependency differs from the generic contract")
	}
}

func TestOptionalPlatformMetadataGoAndSchema(t *testing.T) {
	var schema map[string]any
	if err := json.Unmarshal(Schema(), &schema); err != nil {
		t.Fatal(err)
	}
	platform := schema["properties"].(map[string]any)["platforms"].(map[string]any)["items"].(map[string]any)["properties"].(map[string]any)
	for field, valid := range map[string]string{
		"containerdVersion": "9.12.34-rc.1+vendor.2", "shimProfile": "7.0-next",
		"containerdDigest": "sha256:" + strings.Repeat("a", 64),
		"runcDigest":       "sha256:" + strings.Repeat("b", 64),
		"runcShimDigest":   "sha256:" + strings.Repeat("c", 64),
	} {
		t.Run(field, func(t *testing.T) {
			pattern := regexp.MustCompile(platform[field].(map[string]any)["pattern"].(string))
			for value, want := range map[string]bool{"": true, valid: true, "../unsafe": false, "bad\nvalue": false, strings.Repeat("a", 129): false} {
				raw := fmt.Sprintf(`{"version":"v1","component":"fixture","platforms":[{"os":"linux","architecture":"arm64","adapter":"custom",%q:%q}]}`, field, value)
				r, err := Decode([]byte(raw))
				if (err == nil) != want || (Validate(r) == nil) != want || pattern.MatchString(value) != want {
					t.Fatalf("Go/schema mismatch for %q: Decode=%v, schema=%v, want=%v", value, err, pattern.MatchString(value), want)
				}
			}
		})
	}
}

func TestSchemaServiceTextSafety(t *testing.T) {
	var schema map[string]any
	if err := json.Unmarshal(Schema(), &schema); err != nil {
		t.Fatal(err)
	}
	service := schema["properties"].(map[string]any)["services"].(map[string]any)["items"].(map[string]any)["properties"].(map[string]any)
	argument := service["arguments"].(map[string]any)["items"].(map[string]any)
	pattern := regexp.MustCompile(argument["pattern"].(string))
	if !pattern.MatchString("worker --name=runner") {
		t.Fatal("schema rejects safe argument text")
	}
	for _, value := range []string{"bad\x00value", "bad\rvalue", "bad\nvalue"} {
		if pattern.MatchString(value) {
			t.Fatal("schema allows control characters")
		}
	}
	env := service["environment"].(map[string]any)["propertyNames"].(map[string]any)
	name := regexp.MustCompile(env["pattern"].(string))
	forbidden := regexp.MustCompile(env["not"].(map[string]any)["pattern"].(string))
	if !name.MatchString("LOG_LEVEL") || name.MatchString("lowercase") || !forbidden.MatchString("LD_PRELOAD") {
		t.Fatal("schema environment safety differs from the format")
	}
}
