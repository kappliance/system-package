package systempackage

import (
	"strings"
	"testing"
)

func TestValidateInputs(t *testing.T) {
	for name, change := range map[string]func(map[string]string, map[string]string, map[string]bool){
		"valid":              func(map[string]string, map[string]string, map[string]bool) {},
		"unknown parameter":  func(p, a map[string]string, s map[string]bool) { p["unknown"] = "value" },
		"empty parameter":    func(p, a map[string]string, s map[string]bool) { p["label"] = "" },
		"parameter controls": func(p, a map[string]string, s map[string]bool) { p["label"] = "bad\nvalue" },
		"parameter template": func(p, a map[string]string, s map[string]bool) { p["label"] = "{{path:run/socket}}" },
		"parameter size":     func(p, a map[string]string, s map[string]bool) { p["label"] = strings.Repeat("a", 4097) },
		"invalid digest":     func(p, a map[string]string, s map[string]bool) { p["digest"] = "sha256:abc" },
		"uppercase digest":   func(p, a map[string]string, s map[string]bool) { p["digest"] = "sha256:" + strings.Repeat("A", 64) },
		"wrong input type":   func(p, a map[string]string, s map[string]bool) { p["credential"] = "value" },
		"missing parameter":  func(p, a map[string]string, s map[string]bool) { delete(p, "label") },
		"missing artifact":   func(p, a map[string]string, s map[string]bool) { delete(a, "payload") },
		"mutable artifact": func(p, a map[string]string, s map[string]bool) {
			a["payload"] = "registry.example.com/apps/payload:latest"
		},
		"implicit registry": func(p, a map[string]string, s map[string]bool) {
			a["payload"] = "apps/payload@sha256:" + strings.Repeat("a", 64)
		},
		"unknown artifact": func(p, a map[string]string, s map[string]bool) { a["unknown"] = a["payload"] },
		"missing secret":   func(p, a map[string]string, s map[string]bool) { delete(s, "credential") },
		"absent secret":    func(p, a map[string]string, s map[string]bool) { s["credential"] = false },
		"unknown secret":   func(p, a map[string]string, s map[string]bool) { s["unknown"] = true },
		"input bounds": func(p, a map[string]string, s map[string]bool) {
			for i := range MaxInputs + 1 {
				p[strings.Repeat("a", i+1)] = "value"
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			digest := "sha256:" + strings.Repeat("a", 64)
			params := map[string]string{"label": "worker-1", "digest": digest}
			artifacts := map[string]string{"payload": "registry.example.com/apps/payload@" + digest}
			secrets := map[string]bool{"credential": true}
			change(params, artifacts, secrets)
			err := ValidateInputs(formatFixture(), params, artifacts, secrets)
			if (err == nil) != (name == "valid") {
				t.Fatalf("unexpected input validation result: %v", err)
			}
		})
	}
	r := Recipe{Inputs: map[string]Input{"optional": {Type: "string"}}}
	if err := ValidateInputs(r, nil, nil, nil); err != nil {
		t.Fatal("optional input required", err)
	}
}
