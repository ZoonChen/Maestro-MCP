package tools

// Capability routing declaration for the J2c tool surface (ADR-009 §2,
// ROLE-CATALOG "工具面分工原则"). Every tool declares the capability
// plane it routes to and the §2 role boundary it consumes; the
// server-exclusive operations are declared here so the contract test
// can prove they never leak into the MCP catalog — the console HITL
// surface owns them, agents only propose.

// Capability planes (frozen vocabulary for the declaration below).
const (
	// PlaneQuery is the read plane: every project role reads the graph
	// and the ledger (the workgraph.read / asset.read family, J4).
	PlaneQuery = "query"
	// PlaneProposal is the developer-level decomposition proposal plane
	// (ADR-009 §2 proposal write; J4 grants workgraph.propose to the
	// developer and coordinator project roles).
	PlaneProposal = "proposal"
	// PlaneLedgerWrite is the ledger registration plane
	// (登记 + 机检, permission asset.register, developer-level).
	PlaneLedgerWrite = "ledger-write"
	// PlaneFunctionalReview is the functional review plane (评审,
	// permission asset.review — technical_lead and qa_owner).
	PlaneFunctionalReview = "functional-review"
	// PlaneFunctionalRelease is the functional release plane (放行,
	// permission asset.approve — the four functional owners,
	// delegation-vetoed by the frozen policy, so an agent can never
	// self-release).
	PlaneFunctionalRelease = "functional-release"
)

// CapabilityRoute declares one tool's capability-plane routing.
type CapabilityRoute struct {
	// Plane is the frozen plane vocabulary above.
	Plane string
	// Boundary cites the ADR-009 §2 sentence the routing consumes.
	Boundary string
	// FunctionalOnly marks planes reserved to functional approvers.
	FunctionalOnly bool
}

// workGraphCapabilityRoutes is the frozen J2c/J4 declaration: tool name →
// capability route. It is the single authority the capability-routing
// contract test enforces against the embedded tool catalog.
var workGraphCapabilityRoutes = map[string]CapabilityRoute{ //nolint:gochecknoglobals // frozen declaration consumed by the contract test
	"worktree_graph_query": {
		Plane:    PlaneQuery,
		Boundary: "结构化状态与证据视图对所有项目角色只读（ADR-009 §4 CP→CO 视图）；scope 出自服务端绑定",
	},
	"decomposition_propose": {
		Plane:    PlaneProposal,
		Boundary: "拆解提案是 developer 级写（J4：workgraph.propose 授 developer/coordinator）；服务端独占校验、封板、改图和状态投影（ADR-009 §2）",
	},
	"asset_register": {
		Plane:    PlaneLedgerWrite,
		Boundary: "登记走机检（类型目录/digest/sensitivity/链形），版本与 owner 服务端推导（ROLE-CATALOG 登记类，developer 级）",
	},
	"asset_review": {
		Plane:          PlaneFunctionalReview,
		Boundary:       "评审 Gate 由职能角色审批（ROLE-CATALOG）；asset.review 授 technical_lead 与 qa_owner",
		FunctionalOnly: true,
	},
	"asset_approve": {
		Plane:          PlaneFunctionalRelease,
		Boundary:       "放行=职能审批且委托主体被否决（asset.approve 在 delegationDeniedActions 上，Agent 永不自批）",
		FunctionalOnly: true,
	},
	"asset_query": {
		Plane:    PlaneQuery,
		Boundary: "台账查询对所有项目角色只读；sensitivity 分级随行返回，confidential 内容永不入库（ADR-009 评审记录 security 自检）",
	},
}

// serverExclusiveOperations are ADR-009 §2 server-exclusive operations:
// validation, sealing, graph mutation and state projection. They must
// NEVER appear as MCP tools; the console HITL surface (seal) and the
// store (validation/mutation/projection) own them.
var serverExclusiveOperations = []string{ //nolint:gochecknoglobals // frozen negative surface, asserted by the contract test
	"seal_plan_revision",
	"mutate_graph_structure",
	"aggregate_node_status",
	"replan_plan_revision",
}
