package openapi3

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func bundleOf(t *testing.T, doc *T, name string) map[string]any {
	t.Helper()

	bundled, err := bundleJSONSchema(doc.Components.Schemas[name].Value)
	require.NoError(t, err)
	return bundled.document.(map[string]any)
}

func validate2020(t *testing.T, schema *Schema, instance string) error {
	t.Helper()

	var value any
	require.NoError(t, json.Unmarshal([]byte(instance), &value))
	return schema.VisitJSON(value, EnableJSONSchema2020())
}

func loadSpec(t *testing.T, spec string) *T {
	t.Helper()

	loader := NewLoader()
	doc, err := loader.LoadFromData([]byte(spec))
	require.NoError(t, err)
	require.NoError(t, doc.Validate(loader.Context))
	return doc
}

func TestBundleLocalRef(t *testing.T) {
	doc := loadSpec(t, `
openapi: 3.1.0
info: {title: t, version: "1"}
paths: {}
components:
  schemas:
    Outer:
      type: object
      unevaluatedProperties: false
      properties:
        detail: {$ref: '#/components/schemas/Detail'}
    Detail:
      type: object
      additionalProperties: false
      properties:
        note: {type: string}
`)

	bundled := bundleOf(t, doc, "Outer")
	require.Equal(t,
		map[string]any{"$ref": "#/$defs/Detail"},
		bundled["properties"].(map[string]any)["detail"])
	require.Contains(t, bundled["$defs"], "Detail")

	outer := doc.Components.Schemas["Outer"].Value
	require.NoError(t, validate2020(t, outer, `{"detail":{"note":"a"}}`))
	require.Error(t, validate2020(t, outer, `{"detail":{"surprise":1}}`))
	// The keyword the built-in validator cannot enforce.
	require.Error(t, validate2020(t, outer, `{"surprise":1}`))
}

func TestBundleRecursiveRef(t *testing.T) {
	doc := loadSpec(t, `
openapi: 3.1.0
info: {title: t, version: "1"}
paths: {}
components:
  schemas:
    Node:
      type: object
      unevaluatedProperties: false
      properties:
        child: {$ref: '#/components/schemas/Node'}
`)

	bundled := bundleOf(t, doc, "Node")
	node := bundled["$defs"].(map[string]any)["Node"].(map[string]any)
	require.Equal(t,
		map[string]any{"$ref": "#/$defs/Node"},
		node["properties"].(map[string]any)["child"],
		"a cycle must reuse the embedded definition rather than expand forever")

	schema := doc.Components.Schemas["Node"].Value
	require.NoError(t, validate2020(t, schema, `{"child":{"child":{}}}`))
	require.Error(t, validate2020(t, schema, `{"child":{"surprise":1}}`))
}

// A $ref into another file names a location that is absent from the root document
// as well as from the schema, so only the loader's resolved target can supply it.
func TestBundleExternalRef(t *testing.T) {
	loader := NewLoader()
	loader.IsExternalRefsAllowed = true
	doc, err := loader.LoadFromFile("testdata/bundledRefs/openapi.yaml")
	require.NoError(t, err)
	require.NoError(t, doc.Validate(loader.Context))

	outer := doc.Components.Schemas["Outer"].Value
	require.NoError(t, validate2020(t, outer, `{"fromCommon":{"fromCommon":"a"},"fromOther":{"fromOther":1}}`))
	require.Error(t, validate2020(t, outer, `{"surprise":1}`))

	// Both files name their schema Item, so the two targets must stay distinct.
	require.Error(t, validate2020(t, outer, `{"fromCommon":{"fromOther":1}}`))
	require.Error(t, validate2020(t, outer, `{"fromOther":{"fromCommon":"a"}}`))

	bundled := bundleOf(t, doc, "Outer")
	require.Len(t, bundled["$defs"], 2)
}

func TestBundleBooleanSchema(t *testing.T) {
	doc := loadSpec(t, `
openapi: 3.1.0
info: {title: t, version: "1"}
paths: {}
components:
  schemas:
    Anything: true
    Nothing: false
    Holder:
      type: object
      unevaluatedProperties: false
      properties:
        any: {$ref: '#/components/schemas/Anything'}
`)

	anything, err := bundleJSONSchema(doc.Components.Schemas["Anything"].Value)
	require.NoError(t, err)
	require.Equal(t, true, anything.document)

	require.NoError(t, validate2020(t, doc.Components.Schemas["Anything"].Value, `{"a":1}`))
	require.Error(t, validate2020(t, doc.Components.Schemas["Nothing"].Value, `{"a":1}`))

	holder := doc.Components.Schemas["Holder"].Value
	require.Equal(t, true, bundleOf(t, doc, "Holder")["$defs"].(map[string]any)["Anything"])
	require.NoError(t, validate2020(t, holder, `{"any":{"whatever":1}}`))
	require.Error(t, validate2020(t, holder, `{"surprise":1}`))
}

// In OpenAPI 3.1 the loader merges keywords written alongside a $ref into the
// resolved target, and only that merged schema carries them.
func TestBundleRefSiblingKeywords(t *testing.T) {
	doc := loadSpec(t, `
openapi: 3.1.0
info: {title: t, version: "1"}
paths: {}
components:
  schemas:
    Holder:
      type: object
      properties:
        capped:
          $ref: '#/components/schemas/Text'
          maxLength: 3
    Text:
      type: string
`)

	holder := doc.Components.Schemas["Holder"].Value
	require.NoError(t, validate2020(t, holder, `{"capped":"abc"}`))
	require.Error(t, validate2020(t, holder, `{"capped":"abcd"}`))
	require.NoError(t, validate2020(t, doc.Components.Schemas["Text"].Value, `"abcd"`))
}

// A $defs name the author already used must not be taken by an embedded target.
func TestBundleDefsNameCollision(t *testing.T) {
	doc := loadSpec(t, `
openapi: 3.1.0
info: {title: t, version: "1"}
paths: {}
components:
  schemas:
    Outer:
      type: object
      additionalProperties: false
      $defs:
        Detail: {type: string}
      properties:
        own: {$ref: '#/components/schemas/Outer/$defs/Detail'}
        shared: {$ref: '#/components/schemas/Detail'}
    Detail:
      type: integer
`)

	bundled := bundleOf(t, doc, "Outer")
	require.Len(t, bundled["$defs"], 3, "the author's Detail must survive alongside both embedded targets")

	outer := doc.Components.Schemas["Outer"].Value
	require.NoError(t, validate2020(t, outer, `{"own":"text","shared":1}`))
	require.Error(t, validate2020(t, outer, `{"own":1}`))
	require.Error(t, validate2020(t, outer, `{"shared":"text"}`))
}

func TestBundleUnresolvedRef(t *testing.T) {
	schema := &Schema{
		Type:       &Types{"object"},
		Properties: Schemas{"broken": &SchemaRef{Ref: "#/components/schemas/Missing"}},
	}

	_, err := bundleJSONSchema(schema)
	require.ErrorContains(t, err, `unresolved reference "#/components/schemas/Missing"`)
}
