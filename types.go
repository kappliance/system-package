// Package systempackage defines the bounded, declarative system package recipe format.
package systempackage

const (
	Version         = "v1"
	MaxRecipeBytes  = 64 << 10
	MaxResources    = 64
	MaxInputs       = 32
	RecipeMediaType = "application/vnd.kappliance.node-installer.recipe.v1+json"
	RawMediaType    = "application/octet-stream"
)

type Recipe struct {
	Version     string            `json:"version"`
	Component   string            `json:"component"`
	Platforms   []Platform        `json:"platforms"`
	Inputs      map[string]Input  `json:"inputs,omitempty"`
	Files       []File            `json:"files,omitempty"`
	Directories []Directory       `json:"directories,omitempty"`
	Services    []Service         `json:"services,omitempty"`
	Sysexts     []SystemExtension `json:"sysexts,omitempty"`
	Containerd  *Containerd       `json:"containerd,omitempty"`
	Pause       *Pause            `json:"pause,omitempty"`
}

// SystemExtension names a raw image layer for system extension installation.
type SystemExtension struct {
	Name string `json:"name"`
	Blob string `json:"blob"`
}

type Platform struct {
	OS                string `json:"os"`
	Architecture      string `json:"architecture"`
	Adapter           string `json:"adapter"`
	ContainerdVersion string `json:"containerdVersion,omitempty"`
	ContainerdDigest  string `json:"containerdDigest,omitempty"`
	RuncDigest        string `json:"runcDigest,omitempty"`
	RuncShimDigest    string `json:"runcShimDigest,omitempty"`
	ConfigVersion     int    `json:"configVersion,omitempty"`
	ShimProfile       string `json:"shimProfile,omitempty"`
}

type Input struct {
	Type     string `json:"type"` // string, digest, artifact, secret
	Required bool   `json:"required"`
	PerNode  bool   `json:"perNode"`
}

type File struct {
	Name       string  `json:"name"`
	Path       string  `json:"path"` // logical root: components, state, run
	Blob       string  `json:"blob,omitempty"`
	Literal    *string `json:"literal,omitempty"`
	Secret     string  `json:"secret,omitempty"`
	Artifact   string  `json:"artifact,omitempty"`
	Executable bool    `json:"executable,omitempty"`
}

type Directory struct {
	Path string `json:"path"`
}

type Service struct {
	Name         string            `json:"name"`
	Binary       string            `json:"binary"`
	Arguments    []string          `json:"arguments,omitempty"`
	Environment  map[string]string `json:"environment,omitempty"`
	Health       Health            `json:"health"`
	RequiresFuse bool              `json:"requiresFuse,omitempty"`
}

type Health struct {
	Snapshotter string `json:"snapshotter,omitempty"`
	Broker      string `json:"broker,omitempty"`
}

type Containerd struct {
	Handler     string            `json:"handler"`
	Snapshotter string            `json:"snapshotter"`
	Socket      string            `json:"socket"`
	ShimBinary  string            `json:"shimBinary"`
	Environment map[string]string `json:"environment,omitempty"`
}

type Pause struct {
	Snapshotter string `json:"snapshotter"`
}
