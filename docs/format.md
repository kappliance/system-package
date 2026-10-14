# System package v1

A package contains one declarative JSON recipe and exactly its referenced raw
blob layers. The recipe is UTF-8 JSON with `version: "v1"`, a `component` name
and one or more `platforms`. Existing v1 field names and media types are retained.
The canonical [JSON Schema](../schema/recipe.schema.json) describes wire fields;
Go validation additionally checks relationships between resources.

## Resources

| Field | Meaning |
|---|---|
| `platforms` | Target OS, architecture and installer adapter; optional containerd build metadata |
| `inputs` | Declared `string`, `digest`, `artifact` or `secret` inputs supplied at installation |
| `files` | A named logical destination with exactly one `blob`, `literal`, `artifact` or `secret` source |
| `directories` | Installer-owned logical `state/` or `run/` directories |
| `services` | Named executable-file references, arguments, environment and optional health sockets |
| `sysexts` | Named systemd extensions referencing raw image blobs |
| `containerd` | Runtime handler, snapshotter, managed socket, shim reference and environment |
| `pause` | Pause-image preparation for the configured snapshotter |

Logical destinations start with `components/`, `state/` or `run/` and cannot
contain absolute paths or traversal. Executable files require a blob and a
`components/` destination. Secret materialization requires a `state/secrets/`
destination; external artifacts require `state/data/`. An installer maps these
logical roots to actual storage and determines ownership and lifecycle.

Inputs must be declared and used. Service arguments and environment values may
reference `{{param:name}}`, `{{file:name}}`, `{{path:state/name}}`,
`{{containerd.socket}}` and `{{pause.diffID}}`. References must resolve within the
recipe. Environment keys beginning with `LD_` and unsafe control characters are
rejected. There are no shell hooks, arbitrary unit directives or archive extraction.
`ValidateInputs` verifies supplied input types and required values. Artifact input
references must include a SHA-256 manifest digest. Registry and secret approval
remain installer policy.

## OCI layout

`pack` writes OCI layout version `1.0.0` with a single image manifest:

- Manifest media type: `application/vnd.oci.image.manifest.v1+json`.
- Config media type: `application/vnd.kappliance.node-installer.recipe.v1+json`.
- Layer media type: `application/octet-stream`.
- Layer annotation `org.opencontainers.image.title`: the recipe's blob name.

The config contains the exact input recipe bytes. Named layers may be referenced
by multiple resources but each name occurs once in the manifest. Packing the same
recipe bytes and named payload bytes produces the same manifest digest. Formatting
changes to the recipe change its digest.

`inspect` verifies local content digests, sizes, recipe validity and named layer
inventory. It does not contact a registry, verify signatures, inspect image
filesystem contents or grant installation authority. Keep private inputs, signing
keys and credentials outside distributed packages.

The local OCI tools bound metadata documents to 1 MiB and individual payloads to
1 GiB. The 64-resource limit also bounds the package to at most 64 named payloads.
Symlinked sources and symlinked layout paths are refused; inputs must be regular
files. Local paths must not contain parent-directory (`..`) components; use a
direct or absolute path. The output directory must be new, and packing failures remove only the
partial output created by that invocation.

## Limits and installer policy

Recipes are limited to 64 KiB, eight platform entries, 32 input declarations and
64 resources in total across files, directories, services and extensions. Strict
decoding rejects duplicate, unknown, wrongly cased and null fields and trailing
JSON. Names, logical paths, argument arrays and environment maps have additional
bounds described by the schema and Go validator.

The library accepts well-formed target platforms without certifying them.
Containerd versions, supported adapters, signed-publisher authority, systemd
capabilities and extension metadata are installer decisions. There is no format
minimum or maximum systemd version. Successful package validation cannot establish
that a particular machine can install or run it.
