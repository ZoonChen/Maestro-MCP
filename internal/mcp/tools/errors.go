package tools

import (
	"encoding/json"
	"fmt"

	"github.com/ZoonChen/Maestro-MCP/internal/publicerror"
	mcp "github.com/mark3labs/mcp-go/mcp"
)

// MaestroError represents a structured error returned by MCP tools.
// Matches the format defined in docs/technical/api-spec.md Section 4.7.
type MaestroError struct {
	Code          string `json:"code"`
	Message       string `json:"message"`
	CorrelationID string `json:"correlation_id"`
	Detail        string `json:"detail,omitempty"`
}

// Error implements the error interface.
func (e MaestroError) Error() string {
	return fmt.Sprintf("[%s] %s", e.Code, e.Message)
}

// maestroToolError creates a structured MCP tool error result from a MaestroError.
func maestroToolError(mae MaestroError) *mcp.CallToolResult {
	if mae.CorrelationID == "" {
		mae.CorrelationID = publicerror.NewCorrelationID()
	}
	// Detail is retained in the Go type for wire compatibility only. Arbitrary
	// diagnostic detail is never safe to return to an MCP client.
	mae.Detail = ""
	payload, _ := json.Marshal(mae) //nolint:errchkjson // MaestroError is a safe struct
	return mcp.NewToolResultError(string(payload))
}

// errorResult maps any error to a structured MCP tool error result.
// Uses errors.Is to correctly match wrapped sentinel errors.
func errorResult(err error) *mcp.CallToolResult {
	public := publicerror.Classify(err)
	publicerror.Log(err, public)
	return maestroToolError(maestroErrorFromPublic(public))
}

// mapError maps a store/service error to a MaestroError with a machine-readable code.
func mapError(err error) MaestroError {
	return maestroErrorFromPublic(publicerror.Classify(err))
}

func maestroErrorFromPublic(public publicerror.Error) MaestroError {
	return MaestroError{
		Code: public.Code, Message: public.Message, CorrelationID: public.CorrelationID,
	}
}
