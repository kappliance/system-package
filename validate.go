package systempackage

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/google/go-containerregistry/pkg/name"
)

var namePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
var digestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
var pathPartPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`)
var environmentPattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)
var inputPattern = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_-]{0,62}$`)
var platformValuePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._+-]{0,127}$`)
var templatePattern = regexp.MustCompile(`\{\{([^{}]+)\}\}`)

func IsDigest(value string) bool { return digestPattern.MatchString(value) }
func IsName(value string) bool   { return namePattern.MatchString(value) }

// StrictDecode rejects duplicate and case-folded keys as well as trailing JSON.
func StrictDecode(data []byte, dst any, limit int) error {
	typ := reflect.TypeOf(dst)
	if typ == nil || typ.Kind() != reflect.Pointer || reflect.ValueOf(dst).IsNil() {
		return errors.New("JSON destination must be a non-nil pointer")
	}
	if len(data) == 0 || len(data) > limit {
		return errors.New("JSON size outside bounds")
	}
	if !utf8.Valid(data) {
		return errors.New("JSON must be valid UTF-8")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := checkTokens(dec); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("trailing JSON")
	}
	var raw any
	if err := json.Unmarshal(data, &raw); err != nil {
		return errors.New("invalid JSON")
	}
	if err := exactFields(raw, typ.Elem()); err != nil {
		return err
	}
	dec = json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return errors.New("invalid JSON field or value")
	}
	return nil
}

func checkTokens(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return errors.New("invalid JSON")
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for dec.More() {
			key, err := dec.Token()
			if err != nil {
				return errors.New("invalid JSON object")
			}
			s, ok := key.(string)
			if !ok || seen[s] {
				return errors.New("duplicate JSON key")
			}
			seen[s] = true
			if err := checkTokens(dec); err != nil {
				return err
			}
		}
	case '[':
		for dec.More() {
			if err := checkTokens(dec); err != nil {
				return err
			}
		}
	default:
		return errors.New("invalid JSON delimiter")
	}
	_, err = dec.Token()
	return err
}

func exactFields(raw any, typ reflect.Type) error {
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if raw == nil {
		return errors.New("null JSON fields are unsupported")
	}
	// Scalar custom JSON encoders such as time.Time are validated by their decoder.
	if typ.Kind() == reflect.Struct {
		object, ok := raw.(map[string]any)
		if !ok {
			return nil
		}
		fields := map[string]reflect.Type{}
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			if f.PkgPath != "" {
				continue
			}
			tag := strings.Split(f.Tag.Get("json"), ",")[0]
			if tag == "" {
				tag = f.Name
			}
			if tag != "-" {
				fields[tag] = f.Type
			}
		}
		for key, value := range object {
			f, ok := fields[key]
			if !ok {
				return fmt.Errorf("unknown JSON field %q", key)
			}
			if err := exactFields(value, f); err != nil {
				return err
			}
		}
	} else if typ.Kind() == reflect.Map {
		if object, ok := raw.(map[string]any); ok {
			for _, value := range object {
				if err := exactFields(value, typ.Elem()); err != nil {
					return err
				}
			}
		}
	} else if typ.Kind() == reflect.Slice || typ.Kind() == reflect.Array {
		if list, ok := raw.([]any); ok {
			for _, value := range list {
				if err := exactFields(value, typ.Elem()); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func Decode(data []byte) (Recipe, error) {
	var r Recipe
	if err := StrictDecode(data, &r, MaxRecipeBytes); err != nil {
		return r, err
	}
	if err := Validate(r); err != nil {
		return r, err
	}
	return r, nil
}

// LogicalPath names only installer-owned component, persistent-state, or run roots.
func LogicalPath(value string) bool {
	if len(value) > 256 || path.Clean(value) != value {
		return false
	}
	parts := strings.Split(value, "/")
	if len(parts) < 2 || !slices.Contains([]string{"components", "state", "run"}, parts[0]) {
		return false
	}
	if parts[0] == "state" && (parts[1] == "binaries" || parts[1] == "inventory.json") {
		return false
	}
	for _, part := range parts[1:] {
		if part == "." || part == ".." || !pathPartPattern.MatchString(part) {
			return false
		}
	}
	return true
}

func textSafe(value string) bool {
	return len(value) <= 4096 && !strings.ContainsAny(value, "\x00\r\n")
}

// Validate checks format semantics without authorizing installation or qualifying a host.
func Validate(r Recipe) error {
	if r.Version != Version || !IsName(r.Component) {
		return errors.New("unsupported recipe version or component")
	}
	if len(r.Platforms) < 1 || len(r.Platforms) > 8 || len(r.Inputs) > MaxInputs || len(r.Files)+len(r.Directories)+len(r.Services)+len(r.Sysexts) > MaxResources {
		return errors.New("recipe resource bounds exceeded")
	}
	seenPlatforms := map[Platform]bool{}
	for _, p := range r.Platforms {
		if seenPlatforms[p] || p.ConfigVersion < 0 {
			return errors.New("invalid or duplicate platform")
		}
		seenPlatforms[p] = true
		for _, value := range []string{p.OS, p.Architecture, p.Adapter} {
			if !platformValuePattern.MatchString(value) {
				return errors.New("invalid platform identifier")
			}
		}
		for _, value := range []string{p.ContainerdVersion, p.ShimProfile} {
			if value != "" && !platformValuePattern.MatchString(value) {
				return errors.New("invalid platform version identifier")
			}
		}
		for _, value := range []string{p.ContainerdDigest, p.RuncDigest, p.RuncShimDigest} {
			if value != "" && !IsDigest(value) {
				return errors.New("invalid platform binary digest")
			}
		}
	}
	used := map[string]bool{}
	extensions := map[string]bool{}
	for _, x := range r.Sysexts {
		if !IsName(x.Name) || !IsName(x.Blob) || extensions[x.Name] {
			return errors.New("invalid or duplicate system extension")
		}
		extensions[x.Name] = true
	}
	for n, in := range r.Inputs {
		if !inputPattern.MatchString(n) || !slices.Contains([]string{"string", "digest", "artifact", "secret"}, in.Type) {
			return errors.New("invalid input declaration")
		}
	}
	files := map[string]File{}
	paths := map[string]bool{}
	for _, f := range r.Files {
		if !IsName(f.Name) || !LogicalPath(f.Path) || files[f.Name].Name != "" || paths[f.Path] {
			return errors.New("invalid or duplicate file name/path")
		}
		files[f.Name] = f
		paths[f.Path] = true
		sources := 0
		if f.Blob != "" {
			sources++
			if !IsName(f.Blob) {
				return errors.New("invalid blob name")
			}
		}
		if f.Literal != nil {
			sources++
			if len(*f.Literal) > 16384 {
				return errors.New("literal is too large")
			}
		}
		if f.Secret != "" {
			sources++
			if r.Inputs[f.Secret].Type != "secret" || !strings.HasPrefix(f.Path, "state/secrets/") {
				return errors.New("invalid secret materialization")
			}
			used[f.Secret] = true
		}
		if f.Artifact != "" {
			sources++
			if r.Inputs[f.Artifact].Type != "artifact" || !strings.HasPrefix(f.Path, "state/data/") {
				return errors.New("invalid artifact materialization")
			}
			used[f.Artifact] = true
		}
		if sources != 1 || f.Executable && (f.Blob == "" || !strings.HasPrefix(f.Path, "components/")) {
			return errors.New("file must have one source; executables require a signed component blob")
		}
	}
	for _, d := range r.Directories {
		if !LogicalPath(d.Path) || paths[d.Path] || strings.HasPrefix(d.Path, "components/") {
			return errors.New("invalid or duplicate directory")
		}
		paths[d.Path] = true
	}
	for filePath := range paths {
		for p := path.Dir(filePath); p != "."; p = path.Dir(p) {
			for _, f := range r.Files {
				if f.Path == p {
					return errors.New("file shadows a parent directory")
				}
			}
		}
	}
	services := map[string]bool{}
	validateTemplate := func(value string) error {
		if !textSafe(value) {
			return errors.New("unsafe service value")
		}
		for _, match := range templatePattern.FindAllStringSubmatch(value, -1) {
			token := match[1]
			switch {
			case strings.HasPrefix(token, "param:"):
				n := strings.TrimPrefix(token, "param:")
				in, ok := r.Inputs[n]
				if !ok || (in.Type != "string" && in.Type != "digest") {
					return errors.New("unknown parameter template")
				}
				used[n] = true
			case strings.HasPrefix(token, "file:"):
				if files[strings.TrimPrefix(token, "file:")].Name == "" {
					return errors.New("unknown file template")
				}
			case strings.HasPrefix(token, "path:"):
				if !LogicalPath(strings.TrimPrefix(token, "path:")) {
					return errors.New("unsafe path template")
				}
			case token == "containerd.socket":
				if r.Containerd == nil {
					return errors.New("containerd socket without configuration")
				}
			case token == "pause.diffID":
				if r.Pause == nil {
					return errors.New("pause DiffID without preparation")
				}
			default:
				return errors.New("unknown template")
			}
		}
		if strings.ContainsAny(templatePattern.ReplaceAllString(value, ""), "{}") {
			return errors.New("malformed template")
		}
		return nil
	}
	for _, s := range r.Services {
		if !IsName(s.Name) || services[s.Name] || !files[s.Binary].Executable || len(s.Arguments) > 64 || len(s.Environment) > 32 {
			return errors.New("invalid service or executable")
		}
		services[s.Name] = true
		for _, a := range s.Arguments {
			if err := validateTemplate(a); err != nil {
				return err
			}
		}
		for n, v := range s.Environment {
			if !environmentPattern.MatchString(n) || strings.HasPrefix(n, "LD_") {
				return errors.New("unsafe service environment")
			}
			if err := validateTemplate(v); err != nil {
				return err
			}
		}
		for _, endpoint := range []string{s.Health.Snapshotter, s.Health.Broker} {
			if endpoint != "" && (!LogicalPath(endpoint) || !strings.HasPrefix(endpoint, "run/")) {
				return errors.New("unsafe health socket")
			}
		}
	}
	if c := r.Containerd; c != nil {
		if !IsName(c.Handler) || !IsName(c.Snapshotter) || !LogicalPath(c.Socket) || !strings.HasPrefix(c.Socket, "run/") || !files[c.ShimBinary].Executable || len(c.Environment) > 32 {
			return errors.New("invalid containerd configuration")
		}
		for n, v := range c.Environment {
			if !environmentPattern.MatchString(n) || strings.HasPrefix(n, "LD_") {
				return errors.New("unsafe containerd environment")
			}
			if err := validateTemplate(v); err != nil {
				return err
			}
		}
	}
	if r.Pause != nil && (r.Containerd == nil || r.Pause.Snapshotter != r.Containerd.Snapshotter) {
		return errors.New("pause preparation requires a matching containerd snapshotter")
	}
	for n := range r.Inputs {
		if !used[n] {
			return fmt.Errorf("unused recipe input %q", n)
		}
	}
	raw, err := json.Marshal(r)
	if err != nil || len(raw) > MaxRecipeBytes {
		return errors.New("recipe encoded size exceeds bounds")
	}
	return nil
}

func ValidateInputs(r Recipe, params, artifacts map[string]string, secrets map[string]bool) error {
	if len(params)+len(artifacts)+len(secrets) > MaxInputs {
		return errors.New("too many inputs")
	}
	for n, v := range params {
		in, ok := r.Inputs[n]
		if !ok || (in.Type != "string" && in.Type != "digest") || !textSafe(v) || v == "" || strings.ContainsAny(v, "{}") || in.Type == "digest" && !IsDigest(v) {
			return errors.New("unknown, mistyped or unsafe parameter")
		}
	}
	for n, v := range artifacts {
		if r.Inputs[n].Type != "artifact" {
			return errors.New("undeclared artifact")
		}
		ref, err := name.NewDigest(v, name.StrictValidation)
		if err != nil || ref.Name() != v || !IsDigest(ref.DigestStr()) {
			return errors.New("artifact must use a manifest digest")
		}
	}
	for n, present := range secrets {
		if r.Inputs[n].Type != "secret" || !present {
			return errors.New("undeclared or absent secret")
		}
	}
	for n, in := range r.Inputs {
		if in.Required {
			switch in.Type {
			case "string", "digest":
				if _, ok := params[n]; !ok {
					return errors.New("required parameter missing")
				}
			case "artifact":
				if _, ok := artifacts[n]; !ok {
					return errors.New("required artifact missing")
				}
			case "secret":
				if !secrets[n] {
					return errors.New("required secret missing")
				}
			}
		}
	}
	return nil
}
