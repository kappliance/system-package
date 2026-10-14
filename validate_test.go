package systempackage

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func formatFixture() Recipe {
	literal := "example configuration"
	return Recipe{
		Version: Version, Component: "fixture",
		Platforms: []Platform{{OS: "linux", Architecture: "arm64", Adapter: "custom-systemd"}},
		Inputs: map[string]Input{
			"label":      {Type: "string", Required: true, PerNode: true},
			"digest":     {Type: "digest", Required: true},
			"payload":    {Type: "artifact", Required: true},
			"credential": {Type: "secret", Required: true},
		},
		Files: []File{
			{Name: "worker", Path: "components/worker", Blob: "worker", Executable: true},
			{Name: "config", Path: "state/data/config", Literal: &literal},
			{Name: "payload", Path: "state/data/payload", Artifact: "payload"},
			{Name: "credential", Path: "state/secrets/token", Secret: "credential"},
		},
		Directories: []Directory{{Path: "state/data/cache"}, {Path: "run/worker"}},
		Services: []Service{{
			Name: "worker", Binary: "worker",
			Arguments:   []string{"--label", "{{param:label}}", "--digest", "{{param:digest}}", "{{file:payload}}", "{{file:credential}}", "{{file:config}}", "{{path:run/worker/api.sock}}"},
			Environment: map[string]string{"LOG_LEVEL": "debug"},
			Health:      Health{Broker: "run/worker/api.sock"},
		}},
		Sysexts: []SystemExtension{{Name: "runtime", Blob: "runtime-image"}},
	}
}

func formatContainerdFixture() Recipe {
	r := formatFixture()
	r.Files = append(r.Files, File{Name: "shim", Path: "components/shim", Blob: "shim", Executable: true})
	r.Containerd = &Containerd{Handler: "custom", Snapshotter: "custom", Socket: "run/custom/snapshotter.sock", ShimBinary: "shim", Environment: map[string]string{}}
	return r
}

func TestGenericRecipes(t *testing.T) {
	for name, change := range map[string]func(*Recipe){
		"arm64": func(*Recipe) {},
		"other platform": func(r *Recipe) {
			r.Platforms[0] = Platform{OS: "freebsd", Architecture: "riscv64", Adapter: "custom-adapter"}
		},
		"future runtime": func(r *Recipe) {
			r.Platforms[0].ContainerdVersion = "9.12.34-rc.1+vendor.2"
			r.Platforms[0].ConfigVersion = 42
			r.Platforms[0].ShimProfile = "7.0-next"
			r.Platforms[0].ContainerdDigest = "sha256:" + strings.Repeat("a", 64)
		},
		"application arguments": func(r *Recipe) {
			r.Services[0].Arguments = append(r.Services[0].Arguments, "--blob-url", "https://example.org/payload")
		},
		"containerd without pause": func(*Recipe) {},
		"matching pause": func(r *Recipe) {
			r.Pause = &Pause{Snapshotter: r.Containerd.Snapshotter}
			r.Services[0].Arguments = append(r.Services[0].Arguments, "{{pause.diffID}}", "{{containerd.socket}}")
		},
		"input only in containerd environment": func(r *Recipe) {
			r.Inputs["runtimeLabel"] = Input{Type: "string", Required: true}
			r.Containerd.Environment["RUNTIME_LABEL"] = "{{param:runtimeLabel}}"
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := formatContainerdFixture()
			change(&r)
			if err := Validate(r); err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := Decode(raw)
			if err != nil {
				t.Fatalf("recipe round trip: %v", err)
			}
			roundTrip, err := json.Marshal(decoded)
			if err != nil || !bytes.Equal(raw, roundTrip) {
				t.Fatalf("recipe wire shape changed: %v", err)
			}
		})
	}
	r := formatFixture()
	if err := Validate(r); err != nil {
		t.Fatal("recipe without containerd", err)
	}
}

func TestContainerdOptionalEnvironmentRoundTrip(t *testing.T) {
	const prefix = `{"version":"v1","component":"fixture","platforms":[{"os":"linux","architecture":"arm64","adapter":"custom"}],"files":[{"name":"shim","path":"components/shim","blob":"shim","executable":true}],"containerd":{"handler":"custom","snapshotter":"custom","socket":"run/custom/socket","shimBinary":"shim"`
	for name, environment := range map[string]string{"nil": "", "empty": `,"environment":{}`} {
		t.Run(name, func(t *testing.T) {
			r, err := Decode([]byte(prefix + environment + `}}`))
			if err != nil {
				t.Fatal("optional environment rejected", err)
			}
			if (r.Containerd.Environment == nil) != (name == "nil") {
				t.Fatal("test did not exercise the intended environment value")
			}
			raw, err := json.Marshal(r)
			if err != nil || bytes.Contains(raw, []byte(`"environment"`)) {
				t.Fatal("empty environment was not omitted", err)
			}
			decoded, err := Decode(raw)
			if err != nil || len(decoded.Containerd.Environment) != 0 {
				t.Fatal("optional environment did not round trip", err)
			}
		})
	}
	if _, err := Decode([]byte(prefix + `,"environment":null}}`)); err == nil {
		t.Fatal("explicit null environment accepted")
	}
}

func TestServiceOptionalArgumentsRoundTrip(t *testing.T) {
	const prefix = `{"version":"v1","component":"fixture","platforms":[{"os":"linux","architecture":"arm64","adapter":"custom"}],"files":[{"name":"worker","path":"components/worker","blob":"worker","executable":true}],"services":[{"name":"worker","binary":"worker"`
	for name, arguments := range map[string]string{"absent": "", "empty": `,"arguments":[]`, "nil": ""} {
		t.Run(name, func(t *testing.T) {
			r, err := Decode([]byte(prefix + arguments + `}]}`))
			if err != nil {
				t.Fatal("optional arguments rejected", err)
			}
			if (r.Services[0].Arguments == nil) != (name != "empty") {
				t.Fatal("test did not exercise the intended argument value")
			}
			if name == "nil" {
				r.Services[0] = Service{Name: "worker", Binary: "worker", Arguments: nil}
			}
			raw, err := json.Marshal(r)
			if err != nil || bytes.Contains(raw, []byte(`"arguments"`)) {
				t.Fatal("empty arguments were not omitted", err)
			}
			decoded, err := Decode(raw)
			if err != nil || len(decoded.Services[0].Arguments) != 0 {
				t.Fatal("optional arguments did not round trip", err)
			}
		})
	}
	if _, err := Decode([]byte(prefix + `,"arguments":null}]}`)); err == nil {
		t.Fatal("explicit null arguments accepted")
	}
}

func TestRejectMalformedRecipes(t *testing.T) {
	for name, change := range map[string]func(*Recipe){
		"version":                                 func(r *Recipe) { r.Version = "v2" },
		"component":                               func(r *Recipe) { r.Component = "../host" },
		"missing platform":                        func(r *Recipe) { r.Platforms = nil },
		"duplicate platform":                      func(r *Recipe) { r.Platforms = append(r.Platforms, r.Platforms[0]) },
		"unsafe platform":                         func(r *Recipe) { r.Platforms[0].OS = "linux/../../host" },
		"oversized identifier":                    func(r *Recipe) { r.Platforms[0].Adapter = strings.Repeat("a", 129) },
		"unsafe version":                          func(r *Recipe) { r.Platforms[0].ContainerdVersion = "9\ncommand" },
		"negative config version":                 func(r *Recipe) { r.Platforms[0].ConfigVersion = -1 },
		"invalid optional digest":                 func(r *Recipe) { r.Platforms[0].RuncDigest = "sha256:abc" },
		"absolute path":                           func(r *Recipe) { r.Files[0].Path = "/etc/shadow" },
		"path traversal":                          func(r *Recipe) { r.Files[0].Path = "components/../executor" },
		"reserved path":                           func(r *Recipe) { r.Files[1].Path = "state/inventory.json" },
		"reserved binaries":                       func(r *Recipe) { r.Files[1].Path = "state/binaries/worker" },
		"file parent":                             func(r *Recipe) { r.Files[1].Path = r.Files[0].Path + "/child" },
		"duplicate destination":                   func(r *Recipe) { r.Files[1].Path = r.Files[0].Path },
		"duplicate file name":                     func(r *Recipe) { r.Files[1].Name = r.Files[0].Name },
		"directory over file":                     func(r *Recipe) { r.Directories[0].Path = r.Files[0].Path },
		"component directory":                     func(r *Recipe) { r.Directories[0].Path = "components/cache" },
		"missing file source":                     func(r *Recipe) { r.Files[1].Literal = nil },
		"multiple file sources":                   func(r *Recipe) { r.Files[0].Literal = r.Files[1].Literal },
		"invalid blob":                            func(r *Recipe) { r.Files[0].Blob = "../worker" },
		"executable literal":                      func(r *Recipe) { r.Files[1].Executable = true },
		"secret outside private state":            func(r *Recipe) { r.Files[3].Path = "run/token" },
		"undeclared secret":                       func(r *Recipe) { r.Files[3].Secret = "unknown" },
		"artifact outside data":                   func(r *Recipe) { r.Files[2].Path = "run/payload" },
		"unused input":                            func(r *Recipe) { r.Inputs["unused"] = Input{Type: "string"} },
		"invalid input name":                      func(r *Recipe) { r.Inputs["../input"] = Input{Type: "string"} },
		"invalid input type":                      func(r *Recipe) { r.Inputs["label"] = Input{Type: "shell"} },
		"unknown binary":                          func(r *Recipe) { r.Services[0].Binary = "missing" },
		"nonexecutable binary":                    func(r *Recipe) { r.Services[0].Binary = "config" },
		"duplicate service":                       func(r *Recipe) { r.Services = append(r.Services, r.Services[0]) },
		"too many arguments":                      func(r *Recipe) { r.Services[0].Arguments = make([]string, 65) },
		"loader environment":                      func(r *Recipe) { r.Services[0].Environment["LD_PRELOAD"] = "worker" },
		"environment name":                        func(r *Recipe) { r.Services[0].Environment["lowercase"] = "value" },
		"environment control character":           func(r *Recipe) { r.Services[0].Environment["LOG_LEVEL"] = "debug\nexec" },
		"argument control character":              func(r *Recipe) { r.Services[0].Arguments[0] = "bad\x00argument" },
		"argument size":                           func(r *Recipe) { r.Services[0].Arguments[0] = strings.Repeat("a", 4097) },
		"unknown template":                        func(r *Recipe) { r.Services[0].Arguments[0] = "{{shell:id}}" },
		"unknown parameter":                       func(r *Recipe) { r.Services[0].Arguments[0] = "{{param:missing}}" },
		"secret parameter":                        func(r *Recipe) { r.Services[0].Arguments[0] = "{{param:credential}}" },
		"unknown file":                            func(r *Recipe) { r.Services[0].Arguments[0] = "{{file:missing}}" },
		"unsafe path template":                    func(r *Recipe) { r.Services[0].Arguments[0] = "{{path:run/../etc}}" },
		"malformed template":                      func(r *Recipe) { r.Services[0].Arguments[0] = "{{param:label}" },
		"health socket":                           func(r *Recipe) { r.Services[0].Health.Broker = "state/broker.sock" },
		"containerd socket without configuration": func(r *Recipe) { r.Services[0].Arguments[0] = "{{containerd.socket}}" },
		"pause without preparation":               func(r *Recipe) { r.Services[0].Arguments[0] = "{{pause.diffID}}" },
		"pause without containerd":                func(r *Recipe) { r.Pause = &Pause{Snapshotter: "custom"} },
		"invalid extension":                       func(r *Recipe) { r.Sysexts[0].Blob = "../image" },
		"duplicate extension":                     func(r *Recipe) { r.Sysexts = append(r.Sysexts, r.Sysexts[0]) },
		"aggregate resources":                     func(r *Recipe) { r.Sysexts = make([]SystemExtension, MaxResources) },
		"input bounds": func(r *Recipe) {
			for i := range MaxInputs + 1 {
				r.Inputs[strings.Repeat("a", i+1)] = Input{Type: "string"}
			}
		},
		"literal bounds": func(r *Recipe) { value := strings.Repeat("a", 16385); r.Files[1].Literal = &value },
		"encoded bounds": func(r *Recipe) {
			value := strings.Repeat("a", 16384)
			for _, name := range []string{"one", "two", "three", "four"} {
				r.Files = append(r.Files, File{Name: name, Path: "state/data/" + name, Literal: &value})
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := formatFixture()
			change(&r)
			if Validate(r) == nil {
				t.Fatal("malformed recipe passed validation")
			}
			raw, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Decode(raw); err == nil {
				t.Fatal("Decode bypassed validation")
			}
		})
	}
}

func TestRejectUnsafeContainerd(t *testing.T) {
	for name, change := range map[string]func(*Recipe){
		"handler":              func(r *Recipe) { r.Containerd.Handler = "../host" },
		"snapshotter":          func(r *Recipe) { r.Containerd.Snapshotter = "unsafe value" },
		"socket":               func(r *Recipe) { r.Containerd.Socket = "state/data/socket" },
		"shim":                 func(r *Recipe) { r.Containerd.ShimBinary = "config" },
		"loader environment":   func(r *Recipe) { r.Containerd.Environment["LD_PRELOAD"] = "worker" },
		"environment name":     func(r *Recipe) { r.Containerd.Environment["invalid"] = "value" },
		"environment value":    func(r *Recipe) { r.Containerd.Environment["RUNTIME_SOCKET"] = "bad\nvalue" },
		"environment template": func(r *Recipe) { r.Containerd.Environment["RUNTIME_SOCKET"] = "{{file:missing}}" },
		"environment bounds": func(r *Recipe) {
			for i := range 33 {
				r.Containerd.Environment[strings.Repeat("A", i+1)] = "value"
			}
		},
		"mismatched pause": func(r *Recipe) { r.Pause = &Pause{Snapshotter: "other"} },
	} {
		t.Run(name, func(t *testing.T) {
			r := formatContainerdFixture()
			change(&r)
			if Validate(r) == nil {
				t.Fatal("unsafe runtime configuration accepted")
			}
		})
	}
}

func TestStrictJSON(t *testing.T) {
	valid := `{"version":"v1","component":"fixture","platforms":[{"os":"linux","architecture":"arm64","adapter":"custom"}]}`
	for _, raw := range []string{
		"", "null", `[]`, `{} {}`, valid + `false`, valid + "junk",
		strings.Replace(valid, `"version":"v1"`, `"version":"v1","version":"v1"`, 1),
		strings.Replace(valid, `"version":"v1"`, `"version":"v1","Version":"v1"`, 1),
		strings.Replace(valid, `"os":"linux"`, `"os":"linux","os":"linux"`, 1),
		strings.Replace(valid, `"os":"linux"`, `"OS":"linux"`, 1),
		strings.Replace(valid, `"component":"fixture"`, `"component":"fixture","operations":["shell"]`, 1),
		strings.Replace(valid, `"os":"linux"`, `"os":"linux","extra":true`, 1),
		strings.Replace(valid, `"component":"fixture"`, `"component":null`, 1),
		strings.Replace(valid, `"component":"fixture"`, `"component":1`, 1),
		strings.Replace(valid, `"component":"fixture"`, `"component":"fixture","files":null`, 1),
		strings.Replace(valid, `"component":"fixture"`, "\"component\":\"fixture\",\"files\":[{\"name\":\"config\",\"path\":\"state/data/config\",\"literal\":\"\xff\"}]", 1),
		strings.Repeat(" ", MaxRecipeBytes+1),
	} {
		if _, err := Decode([]byte(raw)); err == nil {
			t.Fatalf("accepted non-strict JSON %q", raw)
		}
	}
	if _, err := Decode([]byte(valid + strings.Repeat(" ", MaxRecipeBytes-len(valid)))); err != nil {
		t.Fatal("valid recipe at the byte limit rejected", err)
	}
	for _, dst := range []any{nil, Recipe{}, (*Recipe)(nil), map[string]any{}} {
		if err := StrictDecode([]byte(valid), dst, MaxRecipeBytes); err == nil {
			t.Fatalf("accepted invalid destination %T", dst)
		}
	}
}

func TestLogicalPaths(t *testing.T) {
	for _, value := range []string{"components/worker", "state/data/payload", "state/secrets/token", "run/worker/api.sock"} {
		if !LogicalPath(value) {
			t.Fatal("safe path rejected", value)
		}
	}
	for _, value := range []string{"", "state", "/etc/shadow", "run/../etc", "state//data", "state/binaries/worker", "state/inventory.json", "state/.hidden", "run/socket\x00", "run/" + strings.Repeat("a", 129)} {
		if LogicalPath(value) {
			t.Fatal("unsafe path accepted", value)
		}
	}
}
