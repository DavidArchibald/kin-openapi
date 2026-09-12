package openapi3filter_test

import (
	"bytes"
	"io"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers/gorillamux"
)

// TestIssue1242UnevaluatedPropertiesWithRef covers the keyword that most depends on
// references resolving: unevaluatedProperties closes an object after allOf
// composition, and the branches it composes are usually $refs.
// See https://github.com/getkin/kin-openapi/issues/1242.
func TestIssue1242UnevaluatedPropertiesWithRef(t *testing.T) {
	spec := []byte(`
openapi: 3.1.0
info:
  title: repro
  version: "1"
paths:
  /probe:
    get:
      operationId: probe
      responses:
        "502":
          description: failure
          content:
            application/json:
              schema:
                $ref: "#/components/schemas/Composed"
components:
  schemas:
    Composed:
      unevaluatedProperties: false
      allOf:
        - $ref: "#/components/schemas/Base"
        - type: object
          required: [extra]
          properties:
            extra: {type: integer}
    Base:
      type: object
      required: [code]
      properties:
        code: {type: string}
`)

	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromData(spec)
	require.NoError(t, err)
	require.NoError(t, doc.Validate(loader.Context))

	router, err := gorillamux.NewRouter(doc)
	require.NoError(t, err)

	req, err := http.NewRequest(http.MethodGet, "http://example.com/probe", nil)
	require.NoError(t, err)
	route, pathParams, err := router.FindRoute(req)
	require.NoError(t, err)

	validate := func(body string) error {
		return openapi3filter.ValidateResponse(loader.Context, &openapi3filter.ResponseValidationInput{
			RequestValidationInput: &openapi3filter.RequestValidationInput{
				Request:    req,
				PathParams: pathParams,
				Route:      route,
			},
			Status: http.StatusBadGateway,
			Header: http.Header{"Content-Type": []string{"application/json"}},
			Body:   io.NopCloser(bytes.NewBufferString(body)),
		})
	}

	require.NoError(t, validate(`{"code":"c","extra":1}`))
	require.ErrorContains(t, validate(`{"code":"c","extra":1,"undeclared":"surprise"}`), "undeclared")
	require.Error(t, validate(`{"extra":1}`))
}
