package openapi3

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
)

var englishPrinter = message.NewPrinter(language.English)

// jsonSchemaValidator wraps the santhosh-tekuri/jsonschema validator
type jsonSchemaValidator struct {
	schema   *jsonschema.Schema
	bundled  *bundledSchema
	settings *schemaValidationSettings
}

// newJSONSchemaValidator creates a new validator using JSON Schema 2020-12
func newJSONSchemaValidator(schema *Schema, settings *schemaValidationSettings) (*jsonSchemaValidator, error) {
	// Convert OpenAPI Schema to JSON Schema format
	bundled, err := bundleJSONSchema(schema)
	if err != nil {
		return nil, fmt.Errorf("failed to bundle schema: %w", err)
	}

	// Create compiler
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)

	// Keep enforcing the formats the built-in validator enforces
	registerFormatValidators(compiler, bundled.document, settings)

	// Add the schema
	schemaURL := "https://example.com/schema.json"
	if err := compiler.AddResource(schemaURL, bundled.document); err != nil {
		return nil, fmt.Errorf("failed to add schema resource: %w", err)
	}

	// Compile the schema
	compiledSchema, err := compiler.Compile(schemaURL)
	if err != nil {
		return nil, fmt.Errorf("failed to compile schema: %w", err)
	}

	return &jsonSchemaValidator{
		schema:   compiledSchema,
		bundled:  bundled,
		settings: settings,
	}, nil
}

func registerFormatValidators(compiler *jsonschema.Compiler, schema any, settings *schemaValidationSettings) {
	formats := make(map[string]struct{})
	collectFormats(schema, formats)
	if len(formats) == 0 {
		return
	}

	for format := range formats {
		compiler.RegisterFormat(&jsonschema.Format{
			Name:     format,
			Validate: formatValidator(format, settings),
		})
	}
	compiler.AssertFormat() // has to be explicitly asserted
}

func collectFormats(node any, formats map[string]struct{}) {
	switch node := node.(type) {
	case map[string]any:
		if format, ok := node["format"].(string); ok && format != "" {
			formats[format] = struct{}{}
		}
		for _, value := range node {
			collectFormats(value, formats)
		}
	case []any:
		for _, value := range node {
			collectFormats(value, formats)
		}
	}
}

func formatValidator(format string, settings *schemaValidationSettings) func(any) error {
	return func(value any) error {
		switch value := value.(type) {
		case string:
			f, ok := settings.stringFormats[format]
			if !ok {
				if f, ok = SchemaStringFormats[format]; !ok {
					return nil
				}
			}
			return f.Validate(value)
		case json.Number:
			if number, err := value.Float64(); err == nil {
				return validateNumberFormat(format, settings, number)
			}
		case float64:
			return validateNumberFormat(format, settings, value)
		case float32:
			return validateNumberFormat(format, settings, float64(value))
		case int:
			return validateNumberFormat(format, settings, float64(value))
		case int32:
			return validateNumberFormat(format, settings, float64(value))
		case int64:
			return validateNumberFormat(format, settings, float64(value))
		}
		return nil
	}
}

func validateNumberFormat(format string, settings *schemaValidationSettings, value float64) error {
	if value == math.Trunc(value) && !math.IsInf(value, 0) {
		f, ok := settings.integerFormats[format]
		if !ok {
			f, ok = SchemaIntegerFormats[format]
		}
		if ok {
			return f.Validate(int64(value))
		}
	}

	f, ok := settings.numberFormats[format]
	if !ok {
		if f, ok = SchemaNumberFormats[format]; !ok {
			return nil
		}
	}
	return f.Validate(value)
}

// transformOpenAPIToJSONSchema converts OpenAPI 3.0/3.1 specific keywords to JSON Schema format
func transformOpenAPIToJSONSchema(schema map[string]any) {
	// Handle nullable - in OpenAPI 3.0, nullable is a boolean flag
	// In OpenAPI 3.1 / JSON Schema 2020-12, we use type arrays
	if nullable, ok := schema["nullable"].(bool); ok && nullable {
		if typeVal, ok := schema["type"].(string); ok {
			// Convert to type array with null
			schema["type"] = []string{typeVal, "null"}
		} else if _, hasType := schema["type"]; !hasType {
			// nullable: true without type - add "null" to allow null values
			schema["type"] = []string{"null"}
		}
		delete(schema, "nullable")
	}

	// Handle exclusiveMinimum/exclusiveMaximum
	// In OpenAPI 3.0, these are booleans alongside minimum/maximum
	// In JSON Schema 2020-12, they are numeric values
	if exclusiveMin, ok := schema["exclusiveMinimum"].(bool); ok {
		if exclusiveMin {
			if schemaMin, ok := schema["minimum"].(float64); ok {
				schema["exclusiveMinimum"] = schemaMin
				delete(schema, "minimum")
			} else {
				delete(schema, "exclusiveMinimum")
			}
		} else {
			// exclusiveMinimum: false means inclusive, which is the JSON Schema default
			delete(schema, "exclusiveMinimum")
		}
	}
	if exclusiveMax, ok := schema["exclusiveMaximum"].(bool); ok {
		if exclusiveMax {
			if schemaMax, ok := schema["maximum"].(float64); ok {
				schema["exclusiveMaximum"] = schemaMax
				delete(schema, "maximum")
			} else {
				delete(schema, "exclusiveMaximum")
			}
		} else {
			// exclusiveMaximum: false means inclusive, which is the JSON Schema default
			delete(schema, "exclusiveMaximum")
		}
	}

	// Remove OpenAPI-specific keywords that aren't in JSON Schema
	delete(schema, "discriminator")
	delete(schema, "xml")
	delete(schema, "externalDocs")
	delete(schema, "example") // Use "examples" in 2020-12

	// Recursively transform nested schemas (single schema fields)
	for _, key := range []string{
		"additionalProperties", "items", "not",
		// OpenAPI 3.1 / JSON Schema 2020-12 fields
		"contains", "propertyNames", "unevaluatedItems", "unevaluatedProperties",
		"if", "then", "else", "contentSchema",
	} {
		if val, ok := schema[key]; ok {
			if nestedSchema, ok := val.(map[string]any); ok {
				transformOpenAPIToJSONSchema(nestedSchema)
			}
		}
	}

	// Transform schema arrays (oneOf, anyOf, allOf, prefixItems)
	for _, key := range []string{"oneOf", "anyOf", "allOf", "prefixItems"} {
		if val, ok := schema[key].([]any); ok {
			for _, item := range val {
				if nestedSchema, ok := item.(map[string]any); ok {
					transformOpenAPIToJSONSchema(nestedSchema)
				}
			}
		}
	}

	// Transform schema maps (properties, patternProperties, dependentSchemas, $defs)
	for _, key := range []string{"properties", "patternProperties", "dependentSchemas", "$defs"} {
		if props, ok := schema[key].(map[string]any); ok {
			for _, propVal := range props {
				if propSchema, ok := propVal.(map[string]any); ok {
					transformOpenAPIToJSONSchema(propSchema)
				}
			}
		}
	}
}

// validate validates a value against the compiled JSON Schema
func (v *jsonSchemaValidator) validate(value any) error {
	if err := v.schema.Validate(value); err != nil {
		// Convert jsonschema error to SchemaError
		return v.convertJSONSchemaError(err, value)
	}
	return nil
}

// convertJSONSchemaError converts a jsonschema validation error to OpenAPI SchemaError format
func (v *jsonSchemaValidator) convertJSONSchemaError(err error, value any) error {
	// TODO: Go 1.26
	// if err, ok := errors.AsType[*jsonschema.ValidationError](err); ok {
	// 	return v.formatValidationError(err, value)
	var validationErr *jsonschema.ValidationError
	if !errors.As(err, &validationErr) {
		return err
	}

	errs := v.formatValidationError(validationErr, value)
	if len(errs) == 1 {
		return errs[0]
	}
	return &SchemaError{
		Value:                 value,
		Schema:                v.bundled.root,
		Origin:                fmt.Errorf("validation failed due to: %w", errs),
		customizeMessageError: v.settings.customizeMessageError,
	}
}

// formatValidationError recursively formats validation errors
func (v *jsonSchemaValidator) formatValidationError(verr *jsonschema.ValidationError, value any) MultiError {
	if len(verr.Causes) == 0 {
		return MultiError{v.schemaError(verr, value)}
	}

	var causes MultiError
	// The validator reaches an object's properties in map order.
	for _, cause := range sortedCauses(verr.Causes) {
		causes = append(causes, v.formatValidationError(cause, value)...)
	}

	// The synthetic root and a $ref hop are not failures of their own.
	switch verr.ErrorKind.(type) {
	case *kind.Schema, *kind.Reference:
		return causes
	}

	err := v.schemaError(verr, value)
	err.Origin = fmt.Errorf("validation failed due to: %w", causes)
	return MultiError{err}
}

func (v *jsonSchemaValidator) schemaError(verr *jsonschema.ValidationError, value any) *SchemaError {
	reversePath := slices.Clone(verr.InstanceLocation)
	slices.Reverse(reversePath)

	err := &SchemaError{
		Value:                 valueAtLocation(value, verr.InstanceLocation),
		reversePath:           reversePath,
		Schema:                v.schemaAt(verr.SchemaURL),
		Reason:                verr.ErrorKind.LocalizedString(englishPrinter),
		customizeMessageError: v.settings.customizeMessageError,
	}
	keywordPath := verr.ErrorKind.KeywordPath()
	if len(keywordPath) > 0 {
		err.SchemaField = keywordPath[len(keywordPath)-1]
	}
	return err
}

// schemaAt resolves a bundled location to the schema it was built from. A keyword
// such as a boolean unevaluatedProperties resolves to the schema declaring it.
func (v *jsonSchemaValidator) schemaAt(schemaURL string) *Schema {
	_, pointer, _ := strings.Cut(schemaURL, "#")
	for pointer != "" {
		schema, ok := v.bundled.locations[pointer]
		if ok {
			return schema
		}
		slash := strings.LastIndexByte(pointer, '/')
		if slash < 0 {
			break
		}
		pointer = pointer[:slash]
	}
	return v.bundled.root
}

func valueAtLocation(value any, location []string) any {
	for _, token := range location {
		switch container := value.(type) {
		case map[string]any:
			value = container[token]
		case []any:
			index, err := strconv.Atoi(token)
			if err != nil || index < 0 || index >= len(container) {
				return nil
			}
			value = container[index]
		default:
			return nil
		}
	}
	return value
}

func sortedCauses(causes []*jsonschema.ValidationError) []*jsonschema.ValidationError {
	sorted := slices.Clone(causes)
	slices.SortStableFunc(sorted, func(a, b *jsonschema.ValidationError) int {
		if c := slices.Compare(a.InstanceLocation, b.InstanceLocation); c != 0 {
			return c
		}
		return slices.Compare(a.ErrorKind.KeywordPath(), b.ErrorKind.KeywordPath())
	})
	return sorted
}

// useJSONSchema2020 validates using the JSON Schema 2020-12 validator
func (schema *Schema) useJSONSchema2020(settings *schemaValidationSettings, value any) error {
	validator, err := newJSONSchemaValidator(schema, settings)
	if err != nil {
		// Fall back to built-in validator if compilation fails
		return schema.visitJSON(settings, value)
	}

	return validator.validate(value)
}
