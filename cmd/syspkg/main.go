// syspkg validates recipes and packages local payloads into offline OCI layouts.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	systempackage "github.com/kappliance/system-package"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "syspkg:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: syspkg validate RECIPE.json | pack --recipe FILE --output DIR [--blob NAME=PATH] | inspect DIR")
	}
	encoder := json.NewEncoder(stdout)
	switch args[0] {
	case "validate":
		if len(args) != 2 {
			return fmt.Errorf("usage: syspkg validate RECIPE.json")
		}
		raw, err := readRecipe(args[1])
		if err != nil {
			return err
		}
		recipe, err := systempackage.Decode(raw)
		if err != nil {
			return err
		}
		return encoder.Encode(recipe)
	case "inspect":
		if len(args) != 2 {
			return fmt.Errorf("usage: syspkg inspect DIR")
		}
		pkg, err := systempackage.Inspect(args[1])
		if err != nil {
			return err
		}
		return encoder.Encode(pkg)
	case "pack":
		flags := flag.NewFlagSet("pack", flag.ContinueOnError)
		flags.SetOutput(stderr)
		recipePath := flags.String("recipe", "", "recipe JSON file")
		output := flags.String("output", "", "new OCI layout directory")
		blobs := blobFlags{}
		flags.Var(&blobs, "blob", "payload NAME=PATH (repeatable)")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if *recipePath == "" || *output == "" || flags.NArg() != 0 {
			return fmt.Errorf("pack requires --recipe FILE --output DIR and no positional arguments")
		}
		raw, err := readRecipe(*recipePath)
		if err != nil {
			return err
		}
		pkg, err := systempackage.Pack(*output, raw, blobs)
		if err != nil {
			return err
		}
		return encoder.Encode(pkg)
	default:
		return fmt.Errorf("unknown command %q; use validate, pack, or inspect", args[0])
	}
}

type blobFlags map[string]string

func (b *blobFlags) String() string { return "NAME=PATH" }
func (b *blobFlags) Set(value string) error {
	name, path, ok := strings.Cut(value, "=")
	if !ok || !systempackage.IsName(name) || path == "" {
		return fmt.Errorf("blob must be NAME=PATH with a valid name and nonempty path")
	}
	if _, exists := (*b)[name]; exists {
		return fmt.Errorf("duplicate blob %q", name)
	}
	(*b)[name] = path
	return nil
}

func readRecipe(path string) ([]byte, error) {
	// Inspect the original spelling before Abs can erase symlink/.. segments.
	for _, part := range strings.FieldsFunc(path, func(r rune) bool {
		return r == '/' || r == rune(filepath.Separator)
	}) {
		if part == ".." {
			return nil, fmt.Errorf("recipe path must not contain '..' components: %q", path)
		}
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	root, err := recipeDirectory(filepath.Dir(absolute))
	if err != nil {
		return nil, err
	}
	defer root.Close()
	leaf := filepath.Base(absolute)
	info, err := root.Lstat(leaf)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("recipe must be a regular file, not a symlink")
	}
	if info.Size() > systempackage.MaxRecipeBytes {
		return nil, fmt.Errorf("recipe exceeds %d bytes", systempackage.MaxRecipeBytes)
	}
	f, err := root.Open(leaf)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	current, err := f.Stat()
	if err != nil || !current.Mode().IsRegular() || !os.SameFile(info, current) {
		return nil, fmt.Errorf("recipe changed while opening")
	}
	raw, err := io.ReadAll(io.LimitReader(f, int64(systempackage.MaxRecipeBytes)+1))
	if err == nil && len(raw) > systempackage.MaxRecipeBytes {
		err = fmt.Errorf("recipe exceeds %d bytes", systempackage.MaxRecipeBytes)
	}
	return raw, err
}

// Walk from an anchored directory so symlinks cannot redirect a recipe read
// between checking the path and opening its parent.
func recipeDirectory(path string) (*os.Root, error) {
	root, err := os.OpenRoot(string(filepath.Separator))
	if err != nil {
		return nil, err
	}
	for _, part := range strings.Split(strings.TrimPrefix(path, string(filepath.Separator)), string(filepath.Separator)) {
		if part == "" {
			continue
		}
		info, err := root.Lstat(part)
		if err != nil {
			root.Close()
			return nil, err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			root.Close()
			return nil, fmt.Errorf("recipe directory must not contain symlinks")
		}
		next, err := root.OpenRoot(part)
		if err != nil {
			root.Close()
			return nil, err
		}
		current, err := next.Stat(".")
		root.Close()
		if err != nil || !os.SameFile(info, current) {
			next.Close()
			return nil, fmt.Errorf("recipe directory changed while opening")
		}
		root = next
	}
	return root, nil
}
