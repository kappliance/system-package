package systempackage

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const packageRecipe = `{
 "version":"v1", "component":"demo",
 "platforms":[{"os":"linux","architecture":"amd64","adapter":"stock-systemd"}],
 "files":[{"name":"message","path":"components/message.txt","blob":"message"}],
 "sysexts":[{"name":"extension","blob":"extension"}]
}
`

func packageInputs(t *testing.T) (string, map[string]string) {
	t.Helper()
	dir := t.TempDir()
	blobs := map[string]string{}
	for name, content := range map[string]string{"message": "hello\n", "extension": "opaque raw image fixture\n"} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		blobs[name] = path
	}
	return dir, blobs
}
func mustPack(t *testing.T) (string, *Package) {
	t.Helper()
	dir, blobs := packageInputs(t)
	path := filepath.Join(dir, "layout")
	pkg, err := Pack(path, []byte(packageRecipe), blobs)
	if err != nil {
		t.Fatal(err)
	}
	return path, pkg
}

func TestOCIRoundtripDeterminism(t *testing.T) {
	dir, blobs := packageInputs(t)
	a, err := Pack(filepath.Join(dir, "one"), []byte(packageRecipe), blobs)
	if err != nil {
		t.Fatal(err)
	}
	// Map insertion order, input location, and mtimes do not affect the image.
	otherDir := t.TempDir()
	reordered := map[string]string{}
	for _, name := range []string{"extension", "message"} {
		data, err := os.ReadFile(blobs[name])
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(otherDir, name)
		if err := os.WriteFile(path, data, 0644); err != nil {
			t.Fatal(err)
		}
		reordered[name] = path
	}
	b, err := Pack(filepath.Join(dir, "two"), []byte(packageRecipe), reordered)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("nondeterministic metadata: %+v != %+v", a, b)
	}
	got, err := Inspect(filepath.Join(dir, "one"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a, got) {
		t.Fatalf("roundtrip: %+v != %+v", a, got)
	}
	if len(got.Layers) != 2 || got.Layers[0].Name != "extension" || got.Layers[1].Name != "message" {
		t.Fatalf("wrong inventory: %+v", got.Layers)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "one", digestPath(got.RecipeDigest)))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, []byte(packageRecipe)) {
		t.Fatal("recipe bytes were rewritten")
	}
	for _, p := range []string{"oci-layout", "index.json", digestPath(a.ManifestDigest), digestPath(a.RecipeDigest)} {
		left, err := os.ReadFile(filepath.Join(dir, "one", p))
		if err != nil {
			t.Fatal(err)
		}
		right, err := os.ReadFile(filepath.Join(dir, "two", p))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(left, right) {
			t.Fatalf("nondeterministic %s", p)
		}
	}
}

func TestOCIPackInventoryAndCleanup(t *testing.T) {
	for _, scenario := range []string{"missing", "extra", "absent-source", "nonregular", "oversize", "symlink", "ancestor-symlink"} {
		t.Run(scenario, func(t *testing.T) {
			dir, blobs := packageInputs(t)
			switch scenario {
			case "missing":
				delete(blobs, "message")
			case "extra":
				blobs["unused"] = blobs["message"]
			case "absent-source":
				blobs["message"] = filepath.Join(dir, "missing")
			case "nonregular":
				blobs["message"] = dir
			case "oversize":
				f, err := os.OpenFile(blobs["message"], os.O_WRONLY, 0600)
				if err != nil {
					t.Fatal(err)
				}
				err = f.Truncate(maxLayerBytes + 1)
				f.Close()
				if err != nil {
					t.Fatal(err)
				}
			case "symlink":
				link := filepath.Join(dir, "link")
				if err := os.Symlink(blobs["message"], link); err != nil {
					t.Fatal(err)
				}
				blobs["message"] = link
			case "ancestor-symlink":
				link := filepath.Join(t.TempDir(), "link")
				if err := os.Symlink(dir, link); err != nil {
					t.Fatal(err)
				}
				blobs["message"] = filepath.Join(link, "message")
			}
			output := filepath.Join(dir, "layout")
			if _, err := Pack(output, []byte(packageRecipe), blobs); err == nil {
				t.Fatal("invalid package accepted")
			}
			if _, err := os.Lstat(output); !os.IsNotExist(err) {
				t.Fatalf("partial output retained: %v", err)
			}
		})
	}
}

func TestOCIPackPreservesExistingOutput(t *testing.T) {
	for _, kind := range []string{"directory", "file", "symlink", "parent-symlink"} {
		t.Run(kind, func(t *testing.T) {
			dir, blobs := packageInputs(t)
			output := filepath.Join(dir, "layout")
			marker := output
			switch kind {
			case "directory":
				if err := os.Mkdir(output, 0700); err != nil {
					t.Fatal(err)
				}
				marker = filepath.Join(output, "keep")
			case "symlink":
				marker = filepath.Join(dir, "keep")
				if err := os.Symlink(marker, output); err != nil {
					t.Fatal(err)
				}
			case "parent-symlink":
				actual := t.TempDir()
				if err := os.Symlink(actual, output); err != nil {
					t.Fatal(err)
				}
				marker = filepath.Join(actual, "keep")
				output = filepath.Join(output, "new-layout")
			}
			if err := os.WriteFile(marker, []byte("preserve"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := Pack(output, []byte(packageRecipe), blobs); err == nil {
				t.Fatal("existing or redirected output accepted")
			}
			raw, err := os.ReadFile(marker)
			if err != nil || string(raw) != "preserve" {
				t.Fatalf("output changed: %s, %v", raw, err)
			}
		})
	}
}

func TestOCIZeroLayersAndSharedPayload(t *testing.T) {
	t.Run("empty-payload", func(t *testing.T) {
		dir, blobs := packageInputs(t)
		if err := os.WriteFile(blobs["message"], nil, 0600); err != nil {
			t.Fatal(err)
		}
		output := filepath.Join(dir, "layout")
		if _, err := Pack(output, []byte(packageRecipe), blobs); err != nil {
			t.Fatal(err)
		}
		if _, err := Inspect(output); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("literal-only", func(t *testing.T) {
		recipe := `{"version":"v1","component":"demo","platforms":[{"os":"linux","architecture":"arm64","adapter":"generic"}],"files":[{"name":"text","path":"components/text","literal":"hello"}]}`
		output := filepath.Join(t.TempDir(), "layout")
		pkg, err := Pack(output, []byte(recipe), nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(pkg.Layers) != 0 {
			t.Fatalf("unexpected payload: %+v", pkg.Layers)
		}
		if _, err := Inspect(output); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("shared-content", func(t *testing.T) {
		dir, blobs := packageInputs(t)
		blobs["extension"] = blobs["message"]
		pkg, err := Pack(filepath.Join(dir, "layout"), []byte(packageRecipe), blobs)
		if err != nil {
			t.Fatal(err)
		}
		if len(pkg.Layers) != 2 || pkg.Layers[0].Digest != pkg.Layers[1].Digest {
			t.Fatal("expected two names sharing one content blob")
		}
		if _, err := Inspect(filepath.Join(dir, "layout")); err != nil {
			t.Fatal(err)
		}
	})
}

// Rewrite authenticated metadata too, so semantic-invalid fixtures reach the
// relevant inventory/descriptor checks rather than merely failing the hash.
func mutateManifest(t *testing.T, layout string, mutate func(*ociManifest)) {
	t.Helper()
	indexPath := filepath.Join(layout, "index.json")
	raw, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	var index ociIndex
	if err := json.Unmarshal(raw, &index); err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile(filepath.Join(layout, digestPath(index.Manifests[0].Digest)))
	if err != nil {
		t.Fatal(err)
	}
	var manifest ociManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	mutate(&manifest)
	raw, err = json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	digest := digestBytes(raw)
	if err := os.WriteFile(filepath.Join(layout, digestPath(digest)), raw, 0600); err != nil {
		t.Fatal(err)
	}
	index.Manifests[0].Digest = digest
	index.Manifests[0].Size = int64(len(raw))
	raw, err = json.Marshal(index)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(indexPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestOCIInspectRejectsCorruption(t *testing.T) {
	for _, target := range []string{"manifest", "recipe", "layer", "layer-size"} {
		t.Run(target, func(t *testing.T) {
			layout, pkg := mustPack(t)
			digest := pkg.ManifestDigest
			switch target {
			case "recipe":
				digest = pkg.RecipeDigest
			case "layer", "layer-size":
				digest = pkg.Layers[0].Digest
			}
			path := filepath.Join(layout, digestPath(digest))
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if target == "layer-size" {
				raw = append(raw, '!')
			} else {
				raw[0] ^= 1
			}
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := Inspect(layout); err == nil {
				t.Fatal("corruption accepted")
			}
		})
	}
}

func TestOCIInspectRejectsInvalidDescriptors(t *testing.T) {
	cases := map[string]func(*ociManifest){
		"missing-layer": func(m *ociManifest) { m.Layers = m.Layers[:1] },
		"extra-layer":   func(m *ociManifest) { m.Layers = append(m.Layers, m.Layers[0]) },
		"duplicate-name": func(m *ociManifest) {
			m.Layers[1].Annotations[titleAnnotation] = m.Layers[0].Annotations[titleAnnotation]
		},
		"unused-name":       func(m *ociManifest) { m.Layers[0].Annotations[titleAnnotation] = "unused" },
		"missing-title":     func(m *ociManifest) { m.Layers[0].Annotations = nil },
		"url":               func(m *ociManifest) { m.Layers[0].URLs = json.RawMessage(`["https://invalid.example/blob"]`) },
		"empty-urls":        func(m *ociManifest) { m.Layers[0].URLs = json.RawMessage(`[]`) },
		"data":              func(m *ociManifest) { m.Config.Data = json.RawMessage(`"aGVsbG8="`) },
		"empty-data":        func(m *ociManifest) { m.Layers[0].Data = json.RawMessage(`""`) },
		"traversal-digest":  func(m *ociManifest) { m.Layers[0].Digest = "sha256:../../outside" },
		"wrong-algorithm":   func(m *ociManifest) { m.Layers[0].Digest = "sha512:" + strings.Repeat("a", 128) },
		"negative-size":     func(m *ociManifest) { m.Layers[0].Size = -1 },
		"zero-size":         func(m *ociManifest) { m.Layers[0].Size = 0 },
		"oversize":          func(m *ociManifest) { m.Layers[0].Size = maxLayerBytes + 1 },
		"oversize-config":   func(m *ociManifest) { m.Config.Size = MaxRecipeBytes + 1 },
		"wrong-config-type": func(m *ociManifest) { m.Config.MediaType = "application/json" },
		"wrong-layer-type":  func(m *ociManifest) { m.Layers[0].MediaType = "application/vnd.oci.image.layer.v1.tar" },
		"no-layers-field":   func(m *ociManifest) { m.Layers = nil },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			layout, _ := mustPack(t)
			mutateManifest(t, layout, mutate)
			if _, err := Inspect(layout); err == nil {
				t.Fatal("invalid descriptor accepted")
			}
		})
	}
}

func TestOCIInspectIndexAndMetadata(t *testing.T) {
	for _, scenario := range []string{"ambiguous", "url", "digest", "size", "duplicate-key", "unknown-key", "layout-version", "metadata-bound"} {
		t.Run(scenario, func(t *testing.T) {
			layout, _ := mustPack(t)
			path := filepath.Join(layout, "index.json")
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var index ociIndex
			if err := json.Unmarshal(raw, &index); err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "ambiguous":
				index.Manifests = append(index.Manifests, index.Manifests[0])
			case "url":
				index.Manifests[0].URLs = json.RawMessage(`["file:///outside"]`)
			case "digest":
				index.Manifests[0].Digest = "../../outside"
			case "size":
				index.Manifests[0].Size = maxMetadataBytes + 1
			case "duplicate-key":
				raw = bytes.Replace(raw, []byte(`"schemaVersion":2`), []byte(`"schemaVersion":2,"schemaVersion":2`), 1)
			case "unknown-key":
				raw = append([]byte(`{"unknown":true,`), raw[1:]...)
			case "layout-version":
				path = filepath.Join(layout, "oci-layout")
				raw = []byte(`{"imageLayoutVersion":"2.0.0"}`)
			case "metadata-bound":
				raw = bytes.Repeat([]byte(" "), maxMetadataBytes+1)
			}
			if scenario == "ambiguous" || scenario == "url" || scenario == "digest" || scenario == "size" {
				raw, err = json.Marshal(index)
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := Inspect(layout); err == nil {
				t.Fatal("invalid metadata accepted")
			}
		})
	}
}

func TestOCIInspectRejectsSymlinksAndNonregular(t *testing.T) {
	for _, scenario := range []string{"layout", "ancestor", "oci-layout", "index.json", "blobs", "sha256", "manifest", "recipe", "layer", "nonregular"} {
		t.Run(scenario, func(t *testing.T) {
			layout, pkg := mustPack(t)
			inspectPath := layout
			relative := scenario
			switch scenario {
			case "layout", "ancestor":
				link := filepath.Join(t.TempDir(), "link")
				target := layout
				if scenario == "ancestor" {
					target = filepath.Dir(layout)
				}
				if err := os.Symlink(target, link); err != nil {
					t.Fatal(err)
				}
				inspectPath = link
				if scenario == "ancestor" {
					inspectPath = filepath.Join(link, filepath.Base(layout))
				}
			default:
				switch scenario {
				case "sha256":
					relative = "blobs/sha256"
				case "manifest":
					relative = digestPath(pkg.ManifestDigest)
				case "recipe":
					relative = digestPath(pkg.RecipeDigest)
				case "layer", "nonregular":
					relative = digestPath(pkg.Layers[0].Digest)
				}
				path := filepath.Join(layout, relative)
				original := filepath.Join(t.TempDir(), "original")
				if err := os.Rename(path, original); err != nil {
					t.Fatal(err)
				}
				if scenario == "nonregular" {
					if err := os.Mkdir(path, 0700); err != nil {
						t.Fatal(err)
					}
				} else if err := os.Symlink(original, path); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := Inspect(inspectPath); err == nil {
				t.Fatal("symlink or nonregular input accepted")
			}
		})
	}
}

func TestOCIRejectsSymlinkParentTraversal(t *testing.T) {
	for _, entry := range []string{"source", "output", "inspect"} {
		t.Run(entry, func(t *testing.T) {
			dir, blobs := packageInputs(t)
			other := filepath.Join(dir, "other")
			if err := os.MkdirAll(filepath.Join(other, "nested"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(other, "nested"), filepath.Join(dir, "link")); err != nil {
				t.Fatal(err)
			}
			intended := []byte("intended OS-resolved payload\n")
			if err := os.WriteFile(filepath.Join(other, "message"), intended, 0600); err != nil {
				t.Fatal(err)
			}
			// Do not use filepath.Join here: it would erase the ambiguity under test.
			ambiguous := dir + "/link/../"
			actual, err := os.ReadFile(ambiguous + "message")
			if err != nil || !bytes.Equal(actual, intended) {
				t.Fatalf("OS path resolution: %q, %v", actual, err)
			}
			wrong, err := os.ReadFile(blobs["message"])
			if err != nil || bytes.Equal(actual, wrong) {
				t.Fatalf("fixture must distinguish intended and wrong sibling: %q, %v", wrong, err)
			}
			output := filepath.Join(dir, "new-layout")
			intendedOutput := filepath.Join(other, "new-layout")
			switch entry {
			case "source":
				blobs["message"] = ambiguous + "message"
				pkg, err := Pack(output, []byte(packageRecipe), blobs)
				if err == nil {
					t.Errorf("ambiguous source accepted; copied wrong sibling: %v", pkg.Layers[1].Digest == digestBytes(wrong))
				}
			case "output":
				if _, err := Pack(ambiguous+"new-layout", []byte(packageRecipe), blobs); err == nil {
					t.Error("ambiguous output accepted")
				}
			case "inspect":
				siblingLayout := filepath.Join(dir, "layout")
				sibling, err := Pack(siblingLayout, []byte(packageRecipe), blobs)
				if err != nil {
					t.Fatal(err)
				}
				blobs["message"] = filepath.Join(other, "message")
				resolved, err := Pack(filepath.Join(other, "layout"), []byte(packageRecipe), blobs)
				if err != nil {
					t.Fatal(err)
				}
				if sibling.ManifestDigest == resolved.ManifestDigest {
					t.Fatal("layouts must differ")
				}
				pkg, err := Inspect(ambiguous + "layout")
				if err == nil {
					t.Errorf("ambiguous inspect accepted; selected wrong sibling: %v", pkg.ManifestDigest == sibling.ManifestDigest)
				}
			}
			for _, path := range []string{output, intendedOutput} {
				if _, err := os.Lstat(path); !os.IsNotExist(err) {
					t.Errorf("unintended output at %s: %v", path, err)
				}
			}
		})
	}
}

func TestOCIRejectsLexicalParentComponents(t *testing.T) {
	dir, blobs := packageInputs(t)
	if err := os.Mkdir(filepath.Join(dir, "nested"), 0700); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(dir, "layout")
	if _, err := Pack(output, []byte(packageRecipe), blobs); err != nil {
		t.Fatal(err)
	}
	ambiguous := dir + "/nested/../"
	if _, err := Inspect(ambiguous + "layout"); err == nil {
		t.Error("inspect accepted lexical parent component")
	}
	blobs["message"] = ambiguous + "message"
	sourceOutput := filepath.Join(dir, "source-output")
	if _, err := Pack(sourceOutput, []byte(packageRecipe), blobs); err == nil {
		t.Error("source accepted lexical parent component")
	}
	blobs["message"] = filepath.Join(dir, "message")
	if _, err := Pack(ambiguous+"output", []byte(packageRecipe), blobs); err == nil {
		t.Error("output accepted lexical parent component")
	}
	for _, path := range []string{sourceOutput, filepath.Join(dir, "output")} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Errorf("unintended output retained: %s, %v", path, err)
		}
	}
}
