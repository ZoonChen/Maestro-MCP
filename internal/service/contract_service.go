package service

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/ZoonChen/Maestro-MCP/internal/model"
	"github.com/ZoonChen/Maestro-MCP/internal/store"
)

// ContractService handles API contract parsing, indexing, and querying.
type ContractService struct {
	contractStore store.ContractStore
}

// NewContractService creates a new ContractService instance.
func NewContractService(contractStore store.ContractStore) *ContractService {
	return &ContractService{
		contractStore: contractStore,
	}
}

// ParseContracts parses contract definitions from the given source file for a project.
// It auto-detects the format (OpenAPI 3.x JSON or manual JSON array) and upserts
// all parsed contracts into the store.
func (s *ContractService) ParseContracts(ctx context.Context, projectID, sourceFile string, _ string) error {
	data, err := os.ReadFile(sourceFile)
	if err != nil {
		return fmt.Errorf("read contract file %s: %w", sourceFile, err)
	}

	trimmed := strings.TrimSpace(string(data))
	if len(trimmed) == 0 {
		return fmt.Errorf("contract file %s is empty", sourceFile)
	}

	var contracts []*model.APIContract

	if strings.HasPrefix(trimmed, "{") {
		contracts, err = s.parseOpenAPIJSON(data, sourceFile)
	} else if strings.HasPrefix(trimmed, "[") {
		contracts, err = s.parseManualJSON(data, sourceFile)
	} else {
		return fmt.Errorf("unsupported contract format in %s: expected JSON object or array", sourceFile)
	}

	if err != nil {
		return fmt.Errorf("parse contract file %s: %w", sourceFile, err)
	}

	now := time.Now().UTC().Format("2006-01-02T15:04:05Z")
	for _, c := range contracts {
		c.ProjectID = projectID
		c.SourceFile = sourceFile
		c.ParsedAt = now
		if err := s.contractStore.Upsert(ctx, projectID, c); err != nil {
			return fmt.Errorf("upsert contract %s %s: %w", c.Method, c.Path, err)
		}
	}

	return nil
}

// parseOpenAPIJSON parses an OpenAPI 3.x JSON document and extracts API contracts
// from the paths object. Only the HTTP methods within each path are extracted;
// parameter and schema details are not parsed in v0.1.
func (s *ContractService) parseOpenAPIJSON(data []byte, _ string) ([]*model.APIContract, error) {
	var doc struct {
		Paths json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}

	var paths map[string]json.RawMessage
	if err := json.Unmarshal(doc.Paths, &paths); err != nil {
		return nil, fmt.Errorf("invalid paths object: %w", err)
	}

	var httpMethods = map[string]bool{
		"get": true, "put": true, "post": true, "delete": true,
		"options": true, "head": true, "patch": true, "trace": true,
	}

	var contracts []*model.APIContract
	for path, methodsRaw := range paths {
		var methods map[string]json.RawMessage
		if err := json.Unmarshal(methodsRaw, &methods); err != nil {
			continue // skip malformed path entries
		}

		for method, operationRaw := range methods {
			if !httpMethods[method] {
				continue
			}

			var op struct {
				Summary     string `json:"summary"`
				Description string `json:"description"`
			}
			_ = json.Unmarshal(operationRaw, &op)

			desc := op.Summary
			if desc == "" {
				desc = op.Description
			}

			c := &model.APIContract{
				Method: strings.ToUpper(method),
				Path:   path,
			}
			if desc != "" {
				c.Description = &desc
			}
			contracts = append(contracts, c)
		}
	}

	if len(contracts) == 0 {
		return nil, fmt.Errorf("no API paths found in OpenAPI document")
	}

	return contracts, nil
}

// parseManualJSON parses a JSON array of contract objects.
// Each object must have "method" and "path" fields; "description",
// "request_schema", and "response_schema" are optional.
func (s *ContractService) parseManualJSON(data []byte, _ string) ([]*model.APIContract, error) {
	var entries []struct {
		Method         string          `json:"method"`
		Path           string          `json:"path"`
		Description    string          `json:"description"`
		RequestSchema  json.RawMessage `json:"request_schema"`
		ResponseSchema json.RawMessage `json:"response_schema"`
	}
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("invalid JSON array: %w", err)
	}

	if len(entries) == 0 {
		return nil, fmt.Errorf("contract array is empty")
	}

	contracts := make([]*model.APIContract, 0, len(entries))
	for i, e := range entries {
		if strings.TrimSpace(e.Method) == "" {
			return nil, fmt.Errorf("entry %d: method is required", i)
		}
		if strings.TrimSpace(e.Path) == "" {
			return nil, fmt.Errorf("entry %d: path is required", i)
		}

		c := &model.APIContract{
			Method: strings.ToUpper(strings.TrimSpace(e.Method)),
			Path:   strings.TrimSpace(e.Path),
		}

		if e.Description != "" {
			c.Description = &e.Description
		}
		if len(e.RequestSchema) > 0 && string(e.RequestSchema) != "null" {
			s := string(e.RequestSchema)
			c.RequestSchema = &s
		}
		if len(e.ResponseSchema) > 0 && string(e.ResponseSchema) != "null" {
			s := string(e.ResponseSchema)
			c.ResponseSchema = &s
		}

		contracts = append(contracts, c)
	}

	return contracts, nil
}

// GetContract retrieves a single API contract by HTTP method and path within a project.
func (s *ContractService) GetContract(ctx context.Context, projectID, method, path string) (*model.APIContract, error) {
	contract, err := s.contractStore.GetByMethodPath(ctx, projectID, method, path)
	if err != nil {
		return nil, fmt.Errorf("get contract %s %s: %w", method, path, err)
	}
	return contract, nil
}

// ListContracts returns all API contracts for a project.
func (s *ContractService) ListContracts(ctx context.Context, projectID string) ([]*model.APIContract, error) {
	contracts, err := s.contractStore.List(ctx, projectID)
	if err != nil {
		return nil, fmt.Errorf("list contracts: %w", err)
	}
	return contracts, nil
}

// UpsertContract inserts or updates a single API contract for a project.
func (s *ContractService) UpsertContract(ctx context.Context, projectID string, c *model.APIContract) error {
	if err := s.contractStore.Upsert(ctx, projectID, c); err != nil {
		return fmt.Errorf("upsert contract: %w", err)
	}
	return nil
}
