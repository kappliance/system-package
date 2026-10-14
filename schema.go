package systempackage

import _ "embed"

//go:embed schema/recipe.schema.json
var recipeSchema []byte

// Schema returns an independent copy of the embedded recipe schema.
func Schema() []byte { return append([]byte(nil), recipeSchema...) }
