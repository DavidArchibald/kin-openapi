package openapi3

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
)

const defsPointer = "/$defs/"

// schemaBundler inlines the $ref targets the loader has resolved. Embedding each
// under the root $defs keeps a recursive schema finite.
type schemaBundler struct {
	document  map[string]any
	names     map[*Schema]string
	locations map[string]*Schema
}

// bundledSchema is a self-contained JSON Schema 2020-12 document. locations gives
// the schema that each sub-schema pointer within it was built from.
type bundledSchema struct {
	document  any
	root      *Schema
	locations map[string]*Schema
}

// bundleJSONSchema returns schema as a self-contained JSON Schema 2020-12 document,
// with the OpenAPI-only keywords already translated.
func bundleJSONSchema(schema *Schema) (*bundledSchema, error) {
	document, err := marshalToJSON(schema)
	if err != nil {
		return nil, err
	}
	encoded, ok := document.(map[string]any)
	if !ok {
		// A boolean schema has no keywords to translate and nothing to embed.
		return &bundledSchema{document: document, root: schema}, nil
	}

	b := schemaBundler{
		document:  encoded,
		names:     make(map[*Schema]string),
		locations: make(map[string]*Schema),
	}
	err = b.walk("", encoded, schema)
	if err != nil {
		return nil, err
	}

	transformOpenAPIToJSONSchema(encoded)
	return &bundledSchema{document: encoded, root: schema, locations: b.locations}, nil
}

// defs returns the $defs object holding the embedded targets. Any $defs the author
// wrote is already in it, so their names are taken for free.
func (b *schemaBundler) defs() map[string]any {
	defs, ok := b.document["$defs"].(map[string]any)
	if !ok {
		defs = make(map[string]any)
		b.document["$defs"] = defs
	}
	return defs
}

// walk replaces the sub-schemas of m, the encoding of s at JSON Pointer ptr, with
// their bundled form.
func (b *schemaBundler) walk(ptr string, m map[string]any, s *Schema) error {
	w := schemaWalk{bundler: b, pointer: ptr, encoded: m}

	w.walkSchema("additionalProperties", s.AdditionalProperties.Schema)
	w.walkSchema("contains", s.Contains)
	w.walkSchema("contentSchema", s.ContentSchema)
	w.walkSchema("else", s.Else)
	w.walkSchema("if", s.If)
	w.walkSchema("items", s.Items)
	w.walkSchema("not", s.Not)
	w.walkSchema("propertyNames", s.PropertyNames)
	w.walkSchema("then", s.Then)
	w.walkSchema("unevaluatedItems", s.UnevaluatedItems.Schema)
	w.walkSchema("unevaluatedProperties", s.UnevaluatedProperties.Schema)

	w.walkArray("allOf", s.AllOf)
	w.walkArray("anyOf", s.AnyOf)
	w.walkArray("oneOf", s.OneOf)
	w.walkArray("prefixItems", s.PrefixItems)

	w.walkMap("$defs", s.Defs)
	w.walkMap("dependentSchemas", s.DependentSchemas)
	w.walkMap("patternProperties", s.PatternProperties)
	w.walkMap("properties", s.Properties)

	return w.err
}

// schemaWalk bundles the sub-schemas of one encoded schema. The first error stops
// every keyword after it.
type schemaWalk struct {
	bundler *schemaBundler
	pointer string
	encoded map[string]any
	err     error
}

func (w *schemaWalk) walkSchema(key string, sr *SchemaRef) {
	if w.err != nil || sr == nil {
		return
	}
	w.encoded[key], w.err = w.bundler.child(w.pointer+"/"+key, w.encoded[key], sr)
}

func (w *schemaWalk) walkArray(key string, refs SchemaRefs) {
	encoded, _ := w.encoded[key].([]any)
	if w.err != nil || len(encoded) != len(refs) {
		return
	}
	for i, ref := range refs {
		encoded[i], w.err = w.bundler.child(w.pointer+"/"+key+"/"+strconv.Itoa(i), encoded[i], ref)
		if w.err != nil {
			return
		}
	}
}

func (w *schemaWalk) walkMap(key string, schemas Schemas) {
	encoded, _ := w.encoded[key].(map[string]any)
	if w.err != nil || encoded == nil {
		return
	}
	for _, name := range slices.Sorted(maps.Keys(schemas)) {
		encoded[name], w.err = w.bundler.child(w.pointer+"/"+key+"/"+escapeRefString(name), encoded[name], schemas[name])
		if w.err != nil {
			return
		}
	}
}

// child returns the bundled encoding of sr. encoded is what sr marshalled to and
// ptr is where it sits in the bundled document.
func (b *schemaBundler) child(ptr string, encoded any, sr *SchemaRef) (any, error) {
	if sr == nil {
		return encoded, nil
	}
	if sr.Value != nil {
		b.locations[ptr] = sr.Value
	}
	if sr.Ref != "" {
		if sr.Value == nil {
			return nil, fmt.Errorf("unresolved reference %q", sr.Ref)
		}
		return b.embed(sr)
	}
	m, isObject := encoded.(map[string]any)
	if !isObject || sr.Value == nil {
		return encoded, nil
	}
	return encoded, b.walk(ptr, m, sr.Value)
}

// embed puts the schema sr points at under $defs and returns a reference to it.
func (b *schemaBundler) embed(sr *SchemaRef) (any, error) {
	name, embedded := b.names[sr.Value]
	if !embedded {
		encoded, err := marshalToJSON(sr.Value)
		if err != nil {
			return nil, err
		}

		defs := b.defs()
		name = uniqueDefsName(defs, sr.Ref)
		b.names[sr.Value] = name
		defs[name] = encoded

		pointer := defsPointer + escapeRefString(name)
		b.locations[pointer] = sr.Value
		m, isObject := encoded.(map[string]any)
		if isObject {
			err = b.walk(pointer, m, sr.Value)
			if err != nil {
				return nil, err
			}
		}
	}
	return map[string]any{"$ref": "#" + defsPointer + escapeRefString(name)}, nil
}

// uniqueDefsName picks the $defs key for a reference. It prefers the last token of
// the reference pointer so validation errors keep naming the author's schema.
func uniqueDefsName(defs map[string]any, ref string) string {
	base := unescapeRefString(ref[strings.LastIndexAny(ref, "/#")+1:])
	if base == "" {
		base = "schema"
	}
	for i, name := 2, base; ; i++ {
		_, taken := defs[name]
		if !taken {
			return name
		}
		name = base + strconv.Itoa(i)
	}
}

// marshalToJSON round-trips v into decoded JSON values. The compiler rejects Go
// values such as *Types and *uint64 with "invalid jsonType".
func marshalToJSON(v any) (any, error) {
	encoded, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var decoded any
	err = json.Unmarshal(encoded, &decoded)
	if err != nil {
		return nil, err
	}
	return decoded, nil
}
