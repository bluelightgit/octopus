package gemini

import (
	"encoding/json"
	"testing"

	transformermodel "github.com/bestruirui/octopus/internal/transformer/model"
)

func TestBuildGeminiResponseSchema_DirectJSONSchema(t *testing.T) {
	format := &transformermodel.ResponseFormat{
		Type: "json_schema",
		JSONSchema: json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}`),
	}

	schema := buildGeminiResponseSchema(format)
	if schema == nil {
		t.Fatal("expected schema")
	}
	if schema.Type != "OBJECT" {
		t.Fatalf("unexpected type: %s", schema.Type)
	}
	if schema.Properties == nil || schema.Properties["name"] == nil {
		t.Fatalf("expected name property")
	}
	if schema.Properties["name"].Type != "STRING" {
		t.Fatalf("unexpected property type: %s", schema.Properties["name"].Type)
	}
}

func TestBuildGeminiResponseSchema_WrappedOpenAISchema(t *testing.T) {
	format := &transformermodel.ResponseFormat{
		Type: "json_schema",
		JSONSchema: json.RawMessage(`{"name":"Result","schema":{"type":"object","properties":{"count":{"type":"integer"}},"required":["count"]},"strict":true}`),
	}

	schema := buildGeminiResponseSchema(format)
	if schema == nil {
		t.Fatal("expected schema")
	}
	if schema.Type != "OBJECT" {
		t.Fatalf("unexpected type: %s", schema.Type)
	}
	if schema.Properties == nil || schema.Properties["count"] == nil {
		t.Fatalf("expected count property")
	}
	if schema.Properties["count"].Type != "INTEGER" {
		t.Fatalf("unexpected property type: %s", schema.Properties["count"].Type)
	}
}
