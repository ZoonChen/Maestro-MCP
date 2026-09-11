package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/ZoonChen/Maestro-MCP/internal/store"
	"github.com/ZoonChen/Maestro-MCP/internal/workgraph"
	mcp "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

// Work Graph and asset-ledger tool surface (J2c, ADR-009 §2/§4). The
// frozen ROLE-CATALOG principle applies: Maestro builds only the
// register / review / release / query tool classes plus the graph read
// and the developer-level proposal write (J4); creation-class tooling
// stays in the agents' own environment. Server-exclusive operations
// (seal, structural mutation, aggregation, replan) are deliberately
// NOT tools: the console HITL surface owns them.

// WorkGraphStore is the read/proposal surface the tools consume (the
// J2a/J2b PostgreSQL store implements it; SQLite deployments mount
// none, which keeps the tools honestly degraded, never fabricated).
type WorkGraphStore interface {
	GetWorkPlan(ctx context.Context, planID string) (*store.WorkPlan, error)
	ListWorkNodes(ctx context.Context, planID string) ([]*store.WorkNode, error)
	CurrentPlanRevision(ctx context.Context, planID string) (*store.PlanRevision, error)
	ListNodeRevisionSpecs(ctx context.Context, planID, revisionID string) ([]*store.WorkNodeRevision, error)
	SubmitDecompositionProposal(ctx context.Context, in store.SubmitDecompositionProposalInput) (*store.DecompositionProposalRecord, error)
}

// AssetStore is the ledger surface the tools consume.
type AssetStore interface {
	RegisterAsset(ctx context.Context, asset store.Asset, actor string) (*store.Asset, error)
	ReviewAsset(ctx context.Context, assetID string, version int, actor string) (*store.Asset, error)
	ApproveAsset(ctx context.Context, assetID string, version int, actor string) (*store.Asset, error)
	GetAsset(ctx context.Context, assetID string, version int) (*store.Asset, error)
	LatestAsset(ctx context.Context, assetID string) (*store.Asset, error)
	ListAssets(ctx context.Context, projectID string) ([]*store.Asset, error)
}

const workGraphStoreReason = "the work graph and asset ledger live in the PostgreSQL control-plane store; this deployment runs without it"

// Wire DTOs: the store row types carry no JSON tags on purpose (storage
// shape); the tool wire contract is snake_case, so the boundary lives
// here and the J2a types stay untouched.
type wireWorkPlan struct {
	ID           string `json:"id"`
	ProjectID    string `json:"project_id"`
	Title        string `json:"title"`
	HumanCode    string `json:"human_code"`
	RootNodeID   string `json:"root_node_id"`
	GraphVersion int64  `json:"graph_version"`
	Status       string `json:"status"`
}

func planWire(plan *store.WorkPlan) wireWorkPlan {
	return wireWorkPlan{
		ID: plan.ID, ProjectID: plan.ProjectID, Title: plan.Title, HumanCode: plan.HumanCode,
		RootNodeID: plan.RootNodeID, GraphVersion: plan.GraphVersion, Status: plan.Status,
	}
}

type wireWorkNode struct {
	ID           string `json:"id"`
	ParentNodeID string `json:"parent_node_id"`
	NodeType     string `json:"node_type"`
	SlotKey      string `json:"slot_key"`
	HumanCode    string `json:"human_code"`
	Status       string `json:"status"`
	NodeVersion  int64  `json:"node_version"`
	Depth        int    `json:"depth"`
}

func nodeWire(node *store.WorkNode) wireWorkNode {
	return wireWorkNode{
		ID: node.ID, ParentNodeID: node.ParentNodeID, NodeType: node.NodeType, SlotKey: node.SlotKey,
		HumanCode: node.HumanCode, Status: node.Status, NodeVersion: node.NodeVersion, Depth: node.Depth,
	}
}

func nodesWire(nodes []*store.WorkNode) []wireWorkNode {
	out := make([]wireWorkNode, 0, len(nodes))
	for _, node := range nodes {
		out = append(out, nodeWire(node))
	}
	return out
}

type wireAsset struct {
	AssetID        string   `json:"asset_id"`
	Version        int      `json:"version"`
	ProjectID      string   `json:"project_id"`
	AssetType      string   `json:"asset_type"`
	Title          string   `json:"title"`
	Status         string   `json:"status"`
	OwnerPrincipal string   `json:"owner_principal"`
	Reviewers      []string `json:"reviewers"`
	Sensitivity    string   `json:"sensitivity"`
	SupersedesRef  string   `json:"supersedes_ref"`
	SourceDigest   string   `json:"source_digest"`
	ContentRef     string   `json:"content_ref"`
	Summary        any      `json:"summary,omitempty"`
	CreatedAt      string   `json:"created_at"`
	ReviewedAt     string   `json:"reviewed_at"`
	ApprovedAt     string   `json:"approved_at"`
}

func assetWire(asset *store.Asset) wireAsset {
	wire := wireAsset{
		AssetID: asset.AssetID, Version: asset.Version, ProjectID: asset.ProjectID,
		AssetType: asset.AssetType, Title: asset.Title, Status: asset.Status,
		OwnerPrincipal: asset.OwnerPrincipal, Reviewers: asset.Reviewers,
		Sensitivity: asset.Sensitivity, SupersedesRef: asset.SupersedesRef,
		SourceDigest: asset.SourceDigest, ContentRef: asset.ContentRef,
		CreatedAt: asset.CreatedAt, ReviewedAt: asset.ReviewedAt, ApprovedAt: asset.ApprovedAt,
	}
	if len(asset.Summary) > 0 {
		wire.Summary = json.RawMessage(asset.Summary)
	}
	return wire
}

// sessionActor derives the audit actor for tool writes: the bound
// session identity, never a payload field.
func (s *Services) sessionActor() (string, error) {
	_, sessionID, _, err := s.Binding.scope()
	if err != nil {
		return "", err
	}
	return "session:" + sessionID, nil
}

// unavailableError is the honest-degradation reply for mutating tools
// whose store is not mounted (SQLite deployments): an explicit stable
// error, never a fabricated write.
func unavailableError() *mcp.CallToolResult {
	return maestroToolError(MaestroError{
		Code:    "OPERATION_DISABLED",
		Message: workGraphStoreReason,
	})
}

// scopeCheckPlan fails closed when the plan lives outside the bound
// project (indistinguishable from absence on the wire).
func scopeCheckPlan(plan *store.WorkPlan, projectID string) error {
	if plan.ProjectID != projectID {
		return fmt.Errorf("%w: plan %s", store.ErrWorkPlanNotFound, plan.ID)
	}
	return nil
}

// RegisterWorkGraphTools registers the six J2c tools on the server.
func RegisterWorkGraphTools(s *mcpserver.MCPServer, services *Services) {
	registerWorktreeGraphQuery(s, services)
	registerDecompositionPropose(s, services)
	registerAssetRegister(s, services)
	registerAssetReview(s, services)
	registerAssetApprove(s, services)
	registerAssetQuery(s, services)
}

// registerWorktreeGraphQuery adds worktree_graph_query (frozen v3.2
// shape): containment tree, node statuses, current revision evidence
// and the per-node JoinPolicy.
func registerWorktreeGraphQuery(s *mcpserver.MCPServer, services *Services) {
	s.AddTool(
		mcp.NewTool("worktree_graph_query",
			mcp.WithDescription("Read one work plan's graph: containment tree, node statuses, current plan revision evidence and per-node JoinPolicy."),
			mcp.WithString("plan_id", mcp.Required(), mcp.Description("Work plan identifier")),
		),
		services.guardTool("worktree_graph_query", func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return handleWorktreeGraphQuery(ctx, req, services)
		}),
	)
}

func handleWorktreeGraphQuery(ctx context.Context, req mcp.CallToolRequest, services *Services) (*mcp.CallToolResult, error) {
	planID, err := req.RequireString("plan_id")
	if err != nil {
		return errorResult(err), nil
	}
	projectID, _, _, err := services.Binding.scope()
	if err != nil {
		return errorResult(err), nil
	}
	if services.WorkGraph == nil {
		payload, marshalErr := json.Marshal(map[string]any{
			"plan_id":   planID,
			"available": false,
			"reason":    workGraphStoreReason,
		})
		if marshalErr != nil {
			return nil, fmt.Errorf("marshal graph query boundary: %w", marshalErr)
		}
		return mcp.NewToolResultText(string(payload)), nil
	}
	plan, err := services.WorkGraph.GetWorkPlan(ctx, planID)
	if err != nil {
		return errorResult(fmt.Errorf("work plan: %w", err)), nil
	}
	if err := scopeCheckPlan(plan, projectID); err != nil {
		return errorResult(err), nil
	}
	nodes, err := services.WorkGraph.ListWorkNodes(ctx, planID)
	if err != nil {
		return errorResult(fmt.Errorf("work nodes: %w", err)), nil
	}

	revision := map[string]any{"available": false}
	nodeSpecs := []map[string]any{}
	if current, revErr := services.WorkGraph.CurrentPlanRevision(ctx, planID); revErr == nil {
		revision = map[string]any{
			"available":     true,
			"id":            current.ID,
			"revision_no":   current.RevisionNo,
			"status":        current.Status,
			"spec_digest":   current.SpecDigest,
			"sealed_at":     current.SealedAt,
			"node_manifest": json.RawMessage(current.NodeManifest),
		}
		specs, specErr := services.WorkGraph.ListNodeRevisionSpecs(ctx, planID, current.ID)
		if specErr != nil {
			return errorResult(fmt.Errorf("node specs: %w", specErr)), nil
		}
		for _, spec := range specs {
			decoded := workgraph.NodeSpec{}
			view := map[string]any{"node_id": spec.NodeID, "spec_digest": spec.SpecDigest}
			if len(spec.Spec) > 0 && json.Unmarshal(spec.Spec, &decoded) == nil {
				view["title"] = decoded.Title
				view["budget_units"] = decoded.BudgetUnits
				view["deadline"] = decoded.Deadline
				view["failure_policy"] = decoded.FailurePolicy
				view["cancel_policy"] = decoded.CancelPolicy
				if decoded.SuccessThreshold != nil {
					view["success_threshold"] = map[string]any{
						"kind": decoded.SuccessThreshold.Kind,
						"k":    decoded.SuccessThreshold.K,
					}
				}
			}
			nodeSpecs = append(nodeSpecs, view)
		}
	}

	payload, err := json.Marshal(map[string]any{
		"plan":             planWire(plan),
		"nodes":            nodesWire(nodes),
		"current_revision": revision,
		"node_specs":       nodeSpecs,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal graph view: %w", err)
	}
	return mcp.NewToolResultText(string(payload)), nil
}

// nodeSpecSchema mirrors the frozen NodeSpec wire (workgraph.NodeSpec).
func nodeSpecSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"title":               map[string]any{"type": "string"},
			"acceptance_criteria": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"repo":                map[string]any{"type": "string"},
			"baseline_sha":        map[string]any{"type": "string"},
			"workspace_paths":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"owning_capability":   map[string]any{"type": "string"},
			"budget_units":        map[string]any{"type": "integer"},
			"deadline":            map[string]any{"type": "string"},
			"priority":            map[string]any{"type": "integer"},
			"required_inputs":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"success_threshold": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"kind": map[string]any{"enum": []string{"all", "any", "quorum"}},
					"k":    map[string]any{"type": "integer"},
				},
			},
			"failure_policy": map[string]any{"enum": []string{"fail_fast", "collect_all", "needs_human"}},
			"cancel_policy":  map[string]any{"enum": []string{"cascade_required", "detach_optional", "none"}},
		},
		"required": []string{"title"},
	}
}

func proposalNodeSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"local_id":   map[string]any{"type": "string"},
			"parent":     map[string]any{"type": "string"},
			"node_type":  map[string]any{"enum": []string{"work_package", "work_item", "gate"}},
			"slot_key":   map[string]any{"type": "string"},
			"human_code": map[string]any{"type": "string"},
			"spec":       nodeSpecSchema(),
		},
		"required": []string{"local_id", "parent", "node_type", "slot_key", "human_code", "spec"},
	}
}

// registerDecompositionPropose adds decomposition_propose (frozen v3.3
// permission, workgraph.propose): the developer-level proposal write
// (developer and coordinator sessions). The server decides
// applied/rejected; violations surface verbatim with their stable codes.
func registerDecompositionPropose(s *mcpserver.MCPServer, services *Services) {
	s.AddTool(
		mcp.NewTool("decomposition_propose",
			mcp.WithDescription("Submit one DecompositionProposal batch to the plan's draft revision behind the graph CAS; the server decides applied/rejected with stable violation codes."),
			mcp.WithString("plan_id", mcp.Required(), mcp.Description("Target work plan")),
			mcp.WithObject("work_pattern", mcp.Required(), mcp.Description("WorkPattern reference (id + version)"),
				mcp.Properties(map[string]any{
					"pattern_id": map[string]any{"type": "string"},
					"version":    map[string]any{"type": "integer"},
				})),
			mcp.WithArray("nodes", mcp.Required(), mcp.Description("1-50 proposal nodes"), mcp.Items(proposalNodeSchema())),
			mcp.WithArray("dependencies", mcp.Description("Optional requires edges"),
				mcp.Items(map[string]any{
					"type": "object",
					"properties": map[string]any{
						"from":        map[string]any{"type": "string"},
						"to":          map[string]any{"type": "string"},
						"requirement": map[string]any{"enum": []string{"required", "optional"}},
					},
					"required": []string{"from", "to"},
				})),
			mcp.WithArray("flows", mcp.Description("Optional consumes/produces edges to ledger assets"),
				mcp.Items(map[string]any{
					"type": "object",
					"properties": map[string]any{
						"node":      map[string]any{"type": "string"},
						"direction": map[string]any{"enum": []string{"consumes", "produces"}},
						"asset_ref": map[string]any{"type": "string"},
						"port_key":  map[string]any{"type": "string"},
					},
					"required": []string{"node", "direction", "asset_ref", "port_key"},
				})),
			mcp.WithNumber("expected_graph_version", mcp.Required(), mcp.Description("Graph CAS token the proposal was built against")),
			mcp.WithString("idempotency_key", mcp.Required(), mcp.Description("16-128 character replay key")),
		),
		services.guardTool("decomposition_propose", func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return handleDecompositionPropose(ctx, req, services)
		}),
	)
}

func handleDecompositionPropose(ctx context.Context, req mcp.CallToolRequest, services *Services) (*mcp.CallToolResult, error) {
	idempotencyKey, err := req.RequireString("idempotency_key")
	if err != nil {
		return errorResult(err), nil
	}
	if !claimIdempotencyKeyPattern.MatchString(idempotencyKey) {
		return errorResult(fmt.Errorf("idempotency_key: %w", store.ErrInvalidParameter)), nil
	}
	if services.WorkGraph == nil {
		return unavailableError(), nil
	}
	projectID, _, _, err := services.Binding.scope()
	if err != nil {
		return errorResult(err), nil
	}

	// The arguments ARE the wire document: round-trip through the typed
	// proposal so every field lands under its domain validation.
	raw, err := json.Marshal(req.GetArguments())
	if err != nil {
		return errorResult(fmt.Errorf("proposal arguments: %w: %v", store.ErrInvalidParameter, err)), nil
	}
	proposal := workgraph.DecompositionProposal{}
	if err := json.Unmarshal(raw, &proposal); err != nil {
		return errorResult(fmt.Errorf("proposal arguments: %w: %v", store.ErrInvalidParameter, err)), nil
	}
	if proposal.PlanID == "" || len(proposal.Nodes) == 0 || proposal.ExpectedGraphVersion < 1 {
		return errorResult(fmt.Errorf("proposal arguments: %w: plan_id, nodes and expected_graph_version are required", store.ErrInvalidParameter)), nil
	}

	// Scope: the target plan must live in the bound project.
	plan, err := services.WorkGraph.GetWorkPlan(ctx, proposal.PlanID)
	if err != nil {
		return errorResult(fmt.Errorf("work plan: %w", err)), nil
	}
	if err := scopeCheckPlan(plan, projectID); err != nil {
		return errorResult(err), nil
	}

	actor, err := services.sessionActor()
	if err != nil {
		return errorResult(err), nil
	}
	record, err := services.WorkGraph.SubmitDecompositionProposal(ctx, store.SubmitDecompositionProposalInput{
		Proposal:       proposal,
		Payload:        raw,
		IdempotencyKey: idempotencyKey,
		SubmittedBy:    actor,
		Limits:         workgraph.ProposalLimits{}, // conservative defaults apply inside the validator
	})
	if err != nil {
		return errorResult(fmt.Errorf("submit proposal: %w", err)), nil
	}
	return proposalRecordResult(record)
}

// proposalRecordResult renders a decided record; a rejection is a
// decided protocol outcome (stable codes on the wire), not a transport
// error.
func proposalRecordResult(record *store.DecompositionProposalRecord) (*mcp.CallToolResult, error) {
	violations := json.RawMessage("null")
	if len(record.Violations) > 0 {
		violations = json.RawMessage(record.Violations)
	}
	applied := json.RawMessage("null")
	if len(record.AppliedNodeIDs) > 0 {
		applied = json.RawMessage(record.AppliedNodeIDs)
	}
	payload, err := json.Marshal(map[string]any{
		"proposal_id":            record.ID,
		"plan_id":                record.PlanID,
		"status":                 record.Status,
		"expected_graph_version": record.ExpectedGraphVersion,
		"violations":             violations,
		"applied_node_ids":       applied,
		"decided_at":             record.DecidedAt,
		"idempotency_key":        record.IdempotencyKey,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal proposal record: %w", err)
	}
	return mcp.NewToolResultText(string(payload)), nil
}

// registerAssetRegister adds asset_register (frozen v3.2 shape): the
// ledger registration write (digest+pointer mode). The server derives
// the version and the owner; machine checks live in the store.
func registerAssetRegister(s *mcpserver.MCPServer, services *Services) {
	s.AddTool(
		mcp.NewTool("asset_register",
			mcp.WithDescription("Register one asset version into the ledger (digest+pointer mode); the server derives the version and owner, machine checks reject invalid registrations."),
			mcp.WithString("asset_id", mcp.Required(), mcp.Description("Stable asset identity, ART-<type>-<number>")),
			mcp.WithString("asset_type", mcp.Required(), mcp.Description("One of the frozen 15-entry type catalog"),
				mcp.Enum(store.AssetTypeCatalog...)),
			mcp.WithString("title", mcp.Required(), mcp.Description("1-200 characters")),
			mcp.WithString("sensitivity", mcp.Required(), mcp.Description("public, internal or confidential"),
				mcp.Enum(store.SensitivityPublic, store.SensitivityInternal, store.SensitivityConfidential)),
			mcp.WithString("source_digest", mcp.Required(), mcp.Description("sha256:<64hex> content digest")),
			mcp.WithString("content_ref", mcp.Description("Content pointer (repository path); required for confidential assets")),
			mcp.WithString("supersedes_ref", mcp.Description("Predecessor version as <asset_id>@<version>")),
			mcp.WithArray("reviewers", mcp.Description("Optional reviewer principals (max 20)")),
			mcp.WithString("summary", mcp.Description("Optional summary (max 4000 characters)")),
			mcp.WithString("idempotency_key", mcp.Required(), mcp.Description("16-128 character replay key")),
		),
		services.guardTool("asset_register", func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return handleAssetRegister(ctx, req, services)
		}),
	)
}

func handleAssetRegister(ctx context.Context, req mcp.CallToolRequest, services *Services) (*mcp.CallToolResult, error) {
	assetID, err := req.RequireString("asset_id")
	if err != nil {
		return errorResult(err), nil
	}
	assetType, err := req.RequireString("asset_type")
	if err != nil {
		return errorResult(err), nil
	}
	title, err := req.RequireString("title")
	if err != nil {
		return errorResult(err), nil
	}
	sensitivity, err := req.RequireString("sensitivity")
	if err != nil {
		return errorResult(err), nil
	}
	sourceDigest, err := req.RequireString("source_digest")
	if err != nil {
		return errorResult(err), nil
	}
	supersedesRef := req.GetString("supersedes_ref", "")
	contentRef := req.GetString("content_ref", "")
	summary := req.GetString("summary", "")
	reviewers, err := requireStringSlice(req, "reviewers", 0, 20)
	if err != nil {
		return errorResult(fmt.Errorf("reviewers: %w", err)), nil
	}
	if _, err := req.RequireString("idempotency_key"); err != nil {
		return errorResult(err), nil
	}
	if services.Assets == nil {
		return unavailableError(), nil
	}
	projectID, _, _, err := services.Binding.scope()
	if err != nil {
		return errorResult(err), nil
	}
	actor, err := services.sessionActor()
	if err != nil {
		return errorResult(err), nil
	}

	// Version derivation is server-side: a first registration is v1;
	// every later version must pin its predecessor (single-successor
	// chain), so the chain shape never depends on caller arithmetic.
	version := 1
	if latest, latestErr := services.Assets.LatestAsset(ctx, assetID); latestErr == nil {
		if supersedesRef == "" {
			return errorResult(fmt.Errorf("%w: asset %s already has version %d; later versions must declare supersedes_ref",
				store.ErrInvalidParameter, assetID, latest.Version)), nil
		}
		version = latest.Version + 1
	} else if !errors.Is(latestErr, store.ErrAssetNotFound) {
		return errorResult(fmt.Errorf("latest asset: %w", latestErr)), nil
	}

	asset := store.Asset{
		AssetID:        assetID,
		Version:        version,
		ProjectID:      projectID,
		AssetType:      assetType,
		Title:          title,
		Status:         store.AssetStatusDraft,
		OwnerPrincipal: actor,
		Reviewers:      reviewers,
		Sensitivity:    sensitivity,
		SupersedesRef:  supersedesRef,
		SourceDigest:   sourceDigest,
		ContentRef:     contentRef,
	}
	if summary != "" {
		asset.Summary = []byte(summary)
	}
	registered, err := services.Assets.RegisterAsset(ctx, asset, actor)
	if err != nil {
		return errorResult(fmt.Errorf("register asset: %w", err)), nil
	}
	payload, err := json.Marshal(assetWire(registered))
	if err != nil {
		return nil, fmt.Errorf("marshal asset: %w", err)
	}
	return mcp.NewToolResultText(string(payload)), nil
}

// separationDeny is the surface-enforced separation of duties (the
// frozen waiver conditions pattern): the registering owner never
// reviews or releases their own asset.
func separationDeny(action string) *mcp.CallToolResult {
	return maestroToolError(MaestroError{
		Code:    "FORBIDDEN",
		Message: "the registering owner may not " + action + " the asset (separation of duties)",
	})
}

func loadScopedAsset(ctx context.Context, services *Services, assetID string, version int) (*store.Asset, string, error) {
	projectID, _, _, err := services.Binding.scope()
	if err != nil {
		return nil, "", err
	}
	asset, err := services.Assets.GetAsset(ctx, assetID, version)
	if err != nil {
		return nil, "", err
	}
	if asset.ProjectID != projectID {
		return nil, "", fmt.Errorf("%w: %s@%d", store.ErrAssetNotFound, assetID, version)
	}
	return asset, projectID, nil
}

func handleAssetTransition(
	ctx context.Context, req mcp.CallToolRequest, services *Services, action string,
	transition func(ctx context.Context, assetID string, version int, actor string) (*store.Asset, error),
) (*mcp.CallToolResult, error) {
	assetID, err := req.RequireString("asset_id")
	if err != nil {
		return errorResult(err), nil
	}
	rawVersion, err := requireIntegerArg(req, "version")
	if err != nil || rawVersion < 1 {
		return errorResult(fmt.Errorf("version: %w", store.ErrInvalidParameter)), nil //nolint:nilerr // parameter validation failed, not the transport
	}
	if _, err := req.RequireString("idempotency_key"); err != nil {
		return errorResult(err), nil
	}
	if services.Assets == nil {
		return unavailableError(), nil
	}
	asset, _, err := loadScopedAsset(ctx, services, assetID, int(rawVersion))
	if err != nil {
		return errorResult(fmt.Errorf("asset: %w", err)), nil
	}
	actor, err := services.sessionActor()
	if err != nil {
		return errorResult(err), nil
	}
	if asset.OwnerPrincipal == actor {
		return separationDeny(action), nil
	}
	updated, err := transition(ctx, assetID, int(rawVersion), actor)
	if err != nil {
		return errorResult(fmt.Errorf("%s asset: %w", action, err)), nil
	}
	payload, err := json.Marshal(assetWire(updated))
	if err != nil {
		return nil, fmt.Errorf("marshal asset: %w", err)
	}
	return mcp.NewToolResultText(string(payload)), nil
}

// registerAssetReview adds asset_review (frozen v3.2 shape).
func registerAssetReview(s *mcpserver.MCPServer, services *Services) {
	s.AddTool(
		mcp.NewTool("asset_review",
			mcp.WithDescription("Record the review pass of one asset version (draft to reviewed); the reviewer may not be the registering owner."),
			mcp.WithString("asset_id", mcp.Required(), mcp.Description("Stable asset identity")),
			mcp.WithNumber("version", mcp.Required(), mcp.Description("Exact asset version")),
			mcp.WithString("idempotency_key", mcp.Required(), mcp.Description("16-128 character replay key")),
		),
		services.guardTool("asset_review", func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			if services.Assets == nil {
				return unavailableError(), nil
			}
			return handleAssetTransition(ctx, req, services, "review",
				func(ctx context.Context, assetID string, version int, actor string) (*store.Asset, error) {
					return services.Assets.ReviewAsset(ctx, assetID, version, actor)
				})
		}),
	)
}

// registerAssetApprove adds asset_approve (frozen v3.2 shape): the
// functional release write; delegated principals are vetoed by the
// frozen policy, the owner by separation of duties.
func registerAssetApprove(s *mcpserver.MCPServer, services *Services) {
	s.AddTool(
		mcp.NewTool("asset_approve",
			mcp.WithDescription("Approve one reviewed asset version (reviewed to approved); predecessors supersede and stale gate bindings flip in the same transaction."),
			mcp.WithString("asset_id", mcp.Required(), mcp.Description("Stable asset identity")),
			mcp.WithNumber("version", mcp.Required(), mcp.Description("Exact asset version")),
			mcp.WithString("idempotency_key", mcp.Required(), mcp.Description("16-128 character replay key")),
		),
		services.guardTool("asset_approve", func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			if services.Assets == nil {
				return unavailableError(), nil
			}
			return handleAssetTransition(ctx, req, services, "approve",
				func(ctx context.Context, assetID string, version int, actor string) (*store.Asset, error) {
					return services.Assets.ApproveAsset(ctx, assetID, version, actor)
				})
		}),
	)
}

// registerAssetQuery adds asset_query (frozen v3.2 shape).
func registerAssetQuery(s *mcpserver.MCPServer, services *Services) {
	s.AddTool(
		mcp.NewTool("asset_query",
			mcp.WithDescription("Query the asset ledger: one asset's full version chain, or a filtered listing by type, sensitivity or lifecycle status."),
			mcp.WithString("asset_id", mcp.Description("Exact asset identity (returns the full version chain)")),
			mcp.WithString("asset_type", mcp.Description("Filter by the frozen type catalog"), mcp.Enum(store.AssetTypeCatalog...)),
			mcp.WithString("sensitivity", mcp.Description("Filter by sensitivity"),
				mcp.Enum(store.SensitivityPublic, store.SensitivityInternal, store.SensitivityConfidential)),
			mcp.WithString("status", mcp.Description("Filter by lifecycle status"),
				mcp.Enum(store.AssetStatusDraft, store.AssetStatusReviewed, store.AssetStatusApproved, store.AssetStatusSuperseded)),
		),
		services.guardTool("asset_query", func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return handleAssetQuery(ctx, req, services)
		}),
	)
}

func handleAssetQuery(ctx context.Context, req mcp.CallToolRequest, services *Services) (*mcp.CallToolResult, error) {
	assetID := req.GetString("asset_id", "")
	assetType := req.GetString("asset_type", "")
	sensitivity := req.GetString("sensitivity", "")
	status := req.GetString("status", "")
	projectID, _, _, err := services.Binding.scope()
	if err != nil {
		return errorResult(err), nil
	}
	if services.Assets == nil {
		payload, marshalErr := json.Marshal(map[string]any{
			"available": false,
			"reason":    workGraphStoreReason,
		})
		if marshalErr != nil {
			return nil, fmt.Errorf("marshal asset query boundary: %w", marshalErr)
		}
		return mcp.NewToolResultText(string(payload)), nil
	}
	assets, err := services.Assets.ListAssets(ctx, projectID)
	if err != nil {
		return errorResult(fmt.Errorf("list assets: %w", err)), nil
	}
	filtered := []*store.Asset{}
	for _, asset := range assets {
		if assetID != "" && asset.AssetID != assetID {
			continue
		}
		if assetType != "" && asset.AssetType != assetType {
			continue
		}
		if sensitivity != "" && asset.Sensitivity != sensitivity {
			continue
		}
		if status != "" && asset.Status != status {
			continue
		}
		filtered = append(filtered, asset)
	}
	sort.Slice(filtered, func(i, j int) bool {
		if filtered[i].AssetID != filtered[j].AssetID {
			return filtered[i].AssetID < filtered[j].AssetID
		}
		return filtered[i].Version < filtered[j].Version
	})
	assetsWire := make([]wireAsset, 0, len(filtered))
	for _, asset := range filtered {
		assetsWire = append(assetsWire, assetWire(asset))
	}
	payload, err := json.Marshal(map[string]any{"assets": assetsWire})
	if err != nil {
		return nil, fmt.Errorf("marshal asset list: %w", err)
	}
	return mcp.NewToolResultText(string(payload)), nil
}
