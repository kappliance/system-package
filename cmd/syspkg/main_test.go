package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	systempackage "github.com/kappliance/system-package"
)

const cliRecipe = `{"version":"v1","component":"demo","platforms":[{"os":"linux","architecture":"amd64","adapter":"stock-systemd"}],"files":[{"name":"message","path":"components/message.txt","blob":"message"}]}`

func cliInputs(t *testing.T) (string, string, string) {
	t.Helper()
	dir := t.TempDir()
	recipe, blob := filepath.Join(dir, "recipe.json"), filepath.Join(dir, "message")
	if err := os.WriteFile(recipe, []byte(cliRecipe), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(blob, []byte("hello\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return recipe, blob, filepath.Join(dir, "layout")
}

func TestCLIRoundtrip(t *testing.T) {
	recipe, blob, output := cliInputs(t)
	var stdout, stderr bytes.Buffer
	if err := run([]string{"validate", recipe}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	var validated systempackage.Recipe
	if err := json.Unmarshal(stdout.Bytes(), &validated); err != nil || validated.Component != "demo" {
		t.Fatalf("validate output %s: %v", stdout.Bytes(), err)
	}
	stdout.Reset()
	if err := run([]string{"pack", "--recipe", recipe, "--output", output, "--blob", "message=" + blob}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	var packed systempackage.Package
	if err := json.Unmarshal(stdout.Bytes(), &packed); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	if err := run([]string{"inspect", output}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	var inspected systempackage.Package
	if err := json.Unmarshal(stdout.Bytes(), &inspected); err != nil {
		t.Fatal(err)
	}
	if packed.ManifestDigest == "" || inspected.ManifestDigest != packed.ManifestDigest || len(inspected.Layers) != 1 {
		t.Fatalf("roundtrip output %s", stdout.Bytes())
	}
	if stderr.Len() != 0 {
		t.Fatalf("unexpected stderr: %s", stderr.String())
	}
}

func TestCLIInvalidArguments(t *testing.T) {
	recipe, blob, output := cliInputs(t)
	for _, args := range [][]string{
		{}, {"unknown"}, {"validate"}, {"inspect"}, {"validate", recipe, "extra"}, {"inspect", output, "extra"},
		{"pack", "--recipe", recipe}, {"pack", "--unknown"}, {"pack", "--recipe", recipe, "--output", output, "extra"},
		{"pack", "--recipe", recipe, "--output", output, "--blob", "message=" + blob, "--blob", "message=" + blob},
		{"pack", "--recipe", recipe, "--output", output, "--blob", "invalid"},
		{"pack", "--recipe", recipe, "--output", output, "--blob", "message="},
		{"pack", "--recipe", recipe, "--output", output},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if err := run(args, &stdout, &stderr); err == nil {
				t.Fatal("invalid command succeeded")
			}
			if stdout.Len() != 0 {
				t.Fatalf("error emitted stdout: %s", stdout.String())
			}
		})
	}
}

func TestCLIRejectsRecipeInput(t *testing.T) {
	for _, scenario := range []string{"invalid", "oversize", "directory", "symlink", "ancestor-symlink"} {
		t.Run(scenario, func(t *testing.T) {
			recipe, _, _ := cliInputs(t)
			switch scenario {
			case "invalid":
				if err := os.WriteFile(recipe, []byte(`{"version":"v1","unknown":true}`), 0600); err != nil {
					t.Fatal(err)
				}
			case "oversize":
				if err := os.WriteFile(recipe, bytes.Repeat([]byte(" "), systempackage.MaxRecipeBytes+1), 0600); err != nil {
					t.Fatal(err)
				}
			case "directory":
				recipe = filepath.Dir(recipe)
			case "symlink", "ancestor-symlink":
				link := filepath.Join(t.TempDir(), "link")
				target := recipe
				if scenario == "ancestor-symlink" {
					target = filepath.Dir(recipe)
				}
				if err := os.Symlink(target, link); err != nil {
					t.Fatal(err)
				}
				recipe = link
				if scenario == "ancestor-symlink" {
					recipe = filepath.Join(link, "recipe.json")
				}
			}
			var stdout, stderr bytes.Buffer
			if err := run([]string{"validate", recipe}, &stdout, &stderr); err == nil {
				t.Fatal("invalid recipe accepted")
			}
			if stdout.Len() != 0 {
				t.Fatal("invalid input emitted JSON")
			}
		})
	}
}

func TestCLIRejectsSymlinkParentTraversal(t *testing.T) {
	for _, entry := range []string{"validate", "pack-recipe", "pack-blob", "pack-output", "inspect"} {
		t.Run(entry, func(t *testing.T) {
			recipe, blob, output := cliInputs(t)
			dir := filepath.Dir(recipe)
			other := filepath.Join(dir, "other")
			if err := os.MkdirAll(filepath.Join(other, "nested"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(other, "nested"), filepath.Join(dir, "link")); err != nil {
				t.Fatal(err)
			}
			actualRecipe := strings.Replace(cliRecipe, `"component":"demo"`, `"component":"actual"`, 1)
			if err := os.WriteFile(filepath.Join(other, "recipe.json"), []byte(actualRecipe), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(other, "message"), []byte("actual payload\n"), 0600); err != nil {
				t.Fatal(err)
			}
			// Preserve the original spelling; filepath.Join would clean away link/...
			ambiguous := dir + "/link/../"
			actual, err := os.ReadFile(ambiguous + "recipe.json")
			if err != nil || string(actual) != actualRecipe || actualRecipe == cliRecipe {
				t.Fatalf("ambiguous recipe fixture: %q, %v", actual, err)
			}
			args := []string{"pack", "--recipe", recipe, "--output", output, "--blob", "message=" + blob}
			switch entry {
			case "validate":
				args = []string{"validate", ambiguous + "recipe.json"}
			case "pack-recipe":
				args[2] = ambiguous + "recipe.json"
			case "pack-blob":
				args[6] = "message=" + ambiguous + "message"
			case "pack-output":
				args[4] = ambiguous + "layout"
			case "inspect":
				if _, err := systempackage.Pack(filepath.Join(dir, "existing"), []byte(cliRecipe), map[string]string{"message": blob}); err != nil {
					t.Fatal(err)
				}
				if _, err := systempackage.Pack(filepath.Join(other, "existing"), []byte(actualRecipe), map[string]string{"message": filepath.Join(other, "message")}); err != nil {
					t.Fatal(err)
				}
				args = []string{"inspect", ambiguous + "existing"}
			}
			var stdout, stderr bytes.Buffer
			if err := run(args, &stdout, &stderr); err == nil {
				t.Error("ambiguous path accepted")
			}
			if stdout.Len() != 0 {
				t.Errorf("ambiguous path emitted JSON: %s", stdout.String())
			}
			for _, path := range []string{output, filepath.Join(other, "layout")} {
				if _, err := os.Lstat(path); !os.IsNotExist(err) {
					t.Errorf("unintended output at %s: %v", path, err)
				}
			}
		})
	}
}

func TestCLIRejectsLexicalParentComponents(t *testing.T) {
	recipe, blob, output := cliInputs(t)
	dir := filepath.Dir(recipe)
	if err := os.Mkdir(filepath.Join(dir, "nested"), 0700); err != nil {
		t.Fatal(err)
	}
	ambiguous := dir + "/nested/../recipe.json"
	for _, args := range [][]string{
		{"validate", ambiguous},
		{"pack", "--recipe", ambiguous, "--output", output, "--blob", "message=" + blob},
	} {
		var stdout, stderr bytes.Buffer
		if err := run(args, &stdout, &stderr); err == nil {
			t.Error("lexical parent component accepted")
		}
		if stdout.Len() != 0 {
			t.Error("lexical parent component emitted JSON")
		}
		if _, err := os.Lstat(output); !os.IsNotExist(err) {
			t.Errorf("unintended output: %v", err)
		}
	}
}
