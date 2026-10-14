package systempackage

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	ociManifestType  = "application/vnd.oci.image.manifest.v1+json"
	ociIndexType     = "application/vnd.oci.image.index.v1+json"
	titleAnnotation  = "org.opencontainers.image.title"
	maxMetadataBytes = 1 << 20
	// At most MaxResources distinct names gives an aggregate bound of 64 GiB.
	maxLayerBytes int64 = 1 << 30
)

// Package describes a verified, offline OCI package. RecipeDigest identifies the
// original recipe bytes, including their whitespace.
type Package struct {
	ManifestDigest string  `json:"manifestDigest"`
	RecipeDigest   string  `json:"recipeDigest"`
	Recipe         Recipe  `json:"recipe"`
	Layers         []Layer `json:"layers"`
}

// Layer identifies a named raw payload without interpreting its contents.
type Layer struct {
	Name   string `json:"name"`
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
}

type ociDescriptor struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Size        int64             `json:"size"`
	Annotations map[string]string `json:"annotations,omitempty"`
	URLs        json.RawMessage   `json:"urls,omitempty"`
	Data        json.RawMessage   `json:"data,omitempty"`
}

func (d *ociDescriptor) UnmarshalJSON(raw []byte) error {
	type descriptor ociDescriptor
	var decoded descriptor
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	if _, ok := fields["size"]; !ok {
		return fmt.Errorf("descriptor requires an explicit size")
	}
	*d = ociDescriptor(decoded)
	return nil
}

type ociIndex struct {
	SchemaVersion int               `json:"schemaVersion"`
	MediaType     string            `json:"mediaType"`
	Manifests     []ociDescriptor   `json:"manifests"`
	Annotations   map[string]string `json:"annotations,omitempty"`
}
type ociManifest struct {
	SchemaVersion int               `json:"schemaVersion"`
	MediaType     string            `json:"mediaType"`
	Config        ociDescriptor     `json:"config"`
	Layers        []ociDescriptor   `json:"layers"`
	Annotations   map[string]string `json:"annotations,omitempty"`
}
type ociLayout struct {
	Version string `json:"imageLayoutVersion"`
}

func recipeBlobNames(r Recipe) []string {
	names := map[string]bool{}
	for _, f := range r.Files {
		if f.Blob != "" {
			names[f.Blob] = true
		}
	}
	for _, s := range r.Sysexts {
		names[s.Blob] = true
	}
	result := make([]string, 0, len(names))
	for name := range names {
		result = append(result, name)
	}
	sort.Strings(result)
	return result
}

// Pack writes one self-contained OCI image layout. The destination must not
// exist. Raw layers are limited to 1 GiB each; no input is executed or extracted.
// Source and output paths must not contain lexical ".." components.
func Pack(output string, recipeJSON []byte, blobs map[string]string) (_ *Package, err error) {
	recipe, err := Decode(recipeJSON)
	if err != nil {
		return nil, fmt.Errorf("recipe: %w", err)
	}
	names := recipeBlobNames(recipe)
	if len(names) != len(blobs) {
		return nil, fmt.Errorf("blob inventory: want %d declared names, got %d", len(names), len(blobs))
	}
	for _, name := range names {
		if _, ok := blobs[name]; !ok {
			return nil, fmt.Errorf("missing blob %q", name)
		}
	}
	parent, leaf, err := openParent(output)
	if err != nil {
		return nil, fmt.Errorf("output parent: %w", err)
	}
	defer parent.Close()
	if err = parent.Mkdir(leaf, 0700); err != nil {
		return nil, fmt.Errorf("create output (must be absent): %w", err)
	}
	identity, err := parent.Lstat(leaf)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err == nil {
			return
		}
		current, statErr := parent.Lstat(leaf)
		if statErr == nil && os.SameFile(identity, current) {
			err = errors.Join(err, parent.RemoveAll(leaf))
		} else if !os.IsNotExist(statErr) {
			err = errors.Join(err, fmt.Errorf("output identity changed; refusing cleanup"))
		}
	}()
	root, err := parent.OpenRoot(leaf)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(identity, opened) {
		return nil, fmt.Errorf("output directory changed while opening")
	}
	if err = root.MkdirAll("blobs/sha256", 0755); err != nil {
		return nil, err
	}
	result := &Package{Recipe: recipe, RecipeDigest: digestBytes(recipeJSON), Layers: make([]Layer, 0, len(names))}
	manifest := ociManifest{SchemaVersion: 2, MediaType: ociManifestType, Config: ociDescriptor{MediaType: RecipeMediaType, Digest: result.RecipeDigest, Size: int64(len(recipeJSON))}, Layers: make([]ociDescriptor, 0, len(names))}
	if err = writeBlob(root, recipeJSON); err != nil {
		return nil, err
	}
	for i, name := range names {
		layer, copyErr := copyLayer(root, fmt.Sprintf(".payload-%d", i), name, blobs[name])
		if copyErr != nil {
			return nil, fmt.Errorf("blob %q: %w", name, copyErr)
		}
		result.Layers = append(result.Layers, layer)
		manifest.Layers = append(manifest.Layers, ociDescriptor{MediaType: RawMediaType, Digest: layer.Digest, Size: layer.Size, Annotations: map[string]string{titleAnnotation: name}})
	}
	rawManifest, err := json.Marshal(manifest)
	if err != nil {
		return nil, err
	}
	if len(rawManifest) > maxMetadataBytes {
		return nil, fmt.Errorf("manifest exceeds metadata limit")
	}
	result.ManifestDigest = digestBytes(rawManifest)
	if err = writeBlob(root, rawManifest); err != nil {
		return nil, err
	}
	index := ociIndex{SchemaVersion: 2, MediaType: ociIndexType, Manifests: []ociDescriptor{{MediaType: ociManifestType, Digest: result.ManifestDigest, Size: int64(len(rawManifest))}}}
	rawIndex, err := json.Marshal(index)
	if err != nil {
		return nil, err
	}
	if err = writeNew(root, "index.json", rawIndex); err != nil {
		return nil, err
	}
	if err = writeNew(root, "oci-layout", []byte(`{"imageLayoutVersion":"1.0.0"}`)); err != nil {
		return nil, err
	}
	return result, nil
}

// Inspect verifies local OCI metadata, recipe bytes, and every declared payload.
// It never follows descriptor URLs or extracts layers. The layout path must not
// contain lexical ".." components.
func Inspect(layoutPath string) (*Package, error) {
	root, err := openDirectory(layoutPath)
	if err != nil {
		return nil, fmt.Errorf("layout: %w", err)
	}
	defer root.Close()
	raw, err := readLocal(root, "oci-layout", maxMetadataBytes)
	if err != nil {
		return nil, err
	}
	var layout ociLayout
	if err = StrictDecode(raw, &layout, maxMetadataBytes); err != nil {
		return nil, fmt.Errorf("oci-layout: %w", err)
	}
	if layout.Version != "1.0.0" {
		return nil, fmt.Errorf("unsupported OCI layout version %q", layout.Version)
	}
	raw, err = readLocal(root, "index.json", maxMetadataBytes)
	if err != nil {
		return nil, err
	}
	var index ociIndex
	if err = StrictDecode(raw, &index, maxMetadataBytes); err != nil {
		return nil, fmt.Errorf("index: %w", err)
	}
	if index.SchemaVersion != 2 || index.MediaType != ociIndexType || len(index.Manifests) != 1 {
		return nil, fmt.Errorf("index must contain exactly one OCI image manifest")
	}
	md := index.Manifests[0]
	raw, err = readDescriptor(root, md, ociManifestType, maxMetadataBytes)
	if err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}
	var manifest ociManifest
	if err = StrictDecode(raw, &manifest, maxMetadataBytes); err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}
	if manifest.SchemaVersion != 2 || manifest.MediaType != ociManifestType || manifest.Layers == nil {
		return nil, fmt.Errorf("expected OCI image manifest schema 2")
	}
	raw, err = readDescriptor(root, manifest.Config, RecipeMediaType, int64(MaxRecipeBytes))
	if err != nil {
		return nil, fmt.Errorf("recipe config: %w", err)
	}
	recipe, err := Decode(raw)
	if err != nil {
		return nil, fmt.Errorf("recipe: %w", err)
	}
	names := recipeBlobNames(recipe)
	if len(names) != len(manifest.Layers) {
		return nil, fmt.Errorf("layer inventory: want %d declared names, got %d", len(names), len(manifest.Layers))
	}
	expected := make(map[string]bool, len(names))
	for _, name := range names {
		expected[name] = true
	}
	result := &Package{ManifestDigest: md.Digest, RecipeDigest: manifest.Config.Digest, Recipe: recipe, Layers: make([]Layer, 0, len(names))}
	for _, d := range manifest.Layers {
		name := d.Annotations[titleAnnotation]
		if !expected[name] {
			return nil, fmt.Errorf("duplicate or undeclared layer name %q", name)
		}
		delete(expected, name)
		if err := checkDescriptor(d, RawMediaType, maxLayerBytes); err != nil {
			return nil, fmt.Errorf("layer %q: %w", name, err)
		}
		f, err := openLocal(root, digestPath(d.Digest))
		if err != nil {
			return nil, fmt.Errorf("layer %q: %w", name, err)
		}
		err = verifyPayload(f, d, io.Discard)
		closeErr := f.Close()
		if err = errors.Join(err, closeErr); err != nil {
			return nil, fmt.Errorf("layer %q: %w", name, err)
		}
		result.Layers = append(result.Layers, Layer{Name: name, Digest: d.Digest, Size: d.Size})
	}
	sort.Slice(result.Layers, func(i, j int) bool { return result.Layers[i].Name < result.Layers[j].Name })
	return result, nil
}

func digestBytes(raw []byte) string {
	h := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(h[:])
}
func digestPath(digest string) string { return "blobs/sha256/" + strings.TrimPrefix(digest, "sha256:") }
func checkDescriptor(d ociDescriptor, mediaType string, limit int64) error {
	if d.MediaType != mediaType {
		return fmt.Errorf("unexpected media type %q", d.MediaType)
	}
	if len(d.URLs) != 0 || len(d.Data) != 0 {
		return fmt.Errorf("descriptor URLs and inline data are forbidden")
	}
	if !IsDigest(d.Digest) || !strings.HasPrefix(d.Digest, "sha256:") {
		return fmt.Errorf("invalid sha256 digest %q", d.Digest)
	}
	if d.Size < 0 || d.Size > limit {
		return fmt.Errorf("descriptor size %d outside 0..%d", d.Size, limit)
	}
	return nil
}
func verifyPayload(f *os.File, d ociDescriptor, dst io.Writer) error {
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() || st.Size() != d.Size {
		return fmt.Errorf("payload is not regular or size differs from descriptor %d", d.Size)
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(h, dst), io.LimitReader(f, d.Size+1))
	if err != nil {
		return err
	}
	if n != d.Size || "sha256:"+hex.EncodeToString(h.Sum(nil)) != d.Digest {
		return fmt.Errorf("payload size or sha256 digest mismatch")
	}
	return nil
}
func readDescriptor(root *os.Root, d ociDescriptor, mediaType string, limit int64) ([]byte, error) {
	if err := checkDescriptor(d, mediaType, limit); err != nil {
		return nil, err
	}
	f, err := openLocal(root, digestPath(d.Digest))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var b strings.Builder
	if err := verifyPayload(f, d, &b); err != nil {
		return nil, err
	}
	return []byte(b.String()), nil
}
func readLocal(root *os.Root, name string, limit int64) ([]byte, error) {
	f, err := openLocal(root, name)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if st.Size() > limit {
		return nil, fmt.Errorf("%s exceeds %d bytes", name, limit)
	}
	raw, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err == nil && int64(len(raw)) > limit {
		err = fmt.Errorf("%s exceeds %d bytes", name, limit)
	}
	return raw, err
}
func writeNew(root *os.Root, path string, raw []byte) error {
	f, err := root.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if err != nil {
		return err
	}
	_, err = f.Write(raw)
	return errors.Join(err, f.Close())
}
func writeBlob(root *os.Root, raw []byte) error {
	path := digestPath(digestBytes(raw))
	if _, err := root.Lstat(path); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	return writeNew(root, path, raw)
}
func copyLayer(root *os.Root, temp, name, source string) (_ Layer, err error) {
	parent, leaf, err := openParent(source)
	if err != nil {
		return Layer{}, err
	}
	defer parent.Close()
	in, err := openLocal(parent, leaf)
	if err != nil {
		return Layer{}, err
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return Layer{}, err
	}
	if st.Size() > maxLayerBytes {
		return Layer{}, fmt.Errorf("payload exceeds %d bytes", maxLayerBytes)
	}
	out, err := root.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if err != nil {
		return Layer{}, err
	}
	h := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(out, h), io.LimitReader(in, maxLayerBytes+1))
	if err = errors.Join(copyErr, out.Close()); err != nil {
		return Layer{}, err
	}
	if n > maxLayerBytes || n != st.Size() {
		return Layer{}, fmt.Errorf("payload size changed or exceeds limit")
	}
	layer := Layer{Name: name, Digest: "sha256:" + hex.EncodeToString(h.Sum(nil)), Size: n}
	target := digestPath(layer.Digest)
	if _, err = root.Lstat(target); err == nil {
		err = root.Remove(temp)
	} else if os.IsNotExist(err) {
		err = root.Rename(temp, target)
	}
	return layer, err
}

// Each directory is opened relative to its already verified parent. Rejecting
// symlinks also keeps callers from accidentally packaging through a redirected
// source path. os.Root confines subsequent reads to that directory.
func openDirectory(path string) (*os.Root, error) {
	if err := rejectParentComponents(path); err != nil {
		return nil, err
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(string(filepath.Separator))
	if err != nil {
		return nil, err
	}
	for _, part := range strings.Split(strings.TrimPrefix(absolute, string(filepath.Separator)), string(filepath.Separator)) {
		if part == "" {
			continue
		}
		st, statErr := root.Lstat(part)
		if statErr != nil {
			root.Close()
			return nil, statErr
		}
		if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
			root.Close()
			return nil, fmt.Errorf("directory %q must be a real directory", part)
		}
		next, openErr := root.OpenRoot(part)
		if openErr != nil {
			root.Close()
			return nil, openErr
		}
		current, statErr := next.Stat(".")
		root.Close()
		if statErr != nil || !os.SameFile(st, current) {
			next.Close()
			return nil, fmt.Errorf("directory %q changed while opening", part)
		}
		root = next
	}
	return root, nil
}
func openParent(path string) (*os.Root, string, error) {
	if err := rejectParentComponents(path); err != nil {
		return nil, "", err
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, "", err
	}
	leaf := filepath.Base(absolute)
	if leaf == "." || leaf == string(filepath.Separator) {
		return nil, "", fmt.Errorf("expected a named path")
	}
	root, err := openDirectory(filepath.Dir(absolute))
	return root, leaf, err
}

// Check the original spelling: Abs/Dir/Clean can erase a symlink followed by
// "..", silently selecting a different file than the operating system would.
func rejectParentComponents(path string) error {
	for _, part := range strings.FieldsFunc(path, func(r rune) bool {
		return r == '/' || r == rune(filepath.Separator)
	}) {
		if part == ".." {
			return fmt.Errorf("path must not contain '..' components: %q", path)
		}
	}
	return nil
}

func openLocal(root *os.Root, path string) (*os.File, error) {
	// OCI paths are digest-derived; the only multi-component local path is the
	// fixed blobs/sha256 directory. Inspect it without following any symlinks.
	parts := strings.Split(path, "/")
	for i := range parts {
		st, err := root.Lstat(strings.Join(parts[:i+1], "/"))
		if err != nil {
			return nil, err
		}
		if st.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("symlink input %q is forbidden", path)
		}
		if i < len(parts)-1 && !st.IsDir() {
			return nil, fmt.Errorf("non-directory input %q", path)
		}
		if i == len(parts)-1 {
			if !st.Mode().IsRegular() {
				return nil, fmt.Errorf("input %q must be a regular file", path)
			}
			f, err := root.Open(path)
			if err != nil {
				return nil, err
			}
			current, err := f.Stat()
			if err != nil || !current.Mode().IsRegular() || !os.SameFile(st, current) {
				f.Close()
				return nil, fmt.Errorf("input %q changed while opening", path)
			}
			return f, nil
		}
	}
	return nil, fmt.Errorf("empty input path")
}
