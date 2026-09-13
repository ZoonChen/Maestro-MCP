// bom-import replays the peixun BOM workbook into Maestro governance
// objects (task brief S2A): the phase-1 44 entries become claimable
// WorkItems on a sealed milestone graph, the phase-2/3 61 entries become
// blocked placeholder nodes, every entry gets a manual Jira anchor, and
// the whole run is idempotent so it can replay safely.
//
// derive.go is the pure derivation layer: the committed manifest (data/
// bom-manifest-20260909.json, digest-anchored into the ART-bom-001
// ledger summary) maps deterministically onto node shapes, slot keys,
// human codes, flat work-item UUIDs and Jira issue keys. No I/O lives
// here — main.go drives the live surfaces.
package main

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// ---------------------------------------------------------------------------
// Deterministic pilot identifiers (the P5b fixed-UUID convention, next
// slots in the series).
// ---------------------------------------------------------------------------

const (
	s2aProjectID   = "018fb5b0-0000-7000-8000-000000000006" // peixun (BOM 治理域)
	s2aTeamID      = "018fb5b0-0000-7000-8000-000000000001" // peixun-pilot (P5b seed)
	s2aClaimRunner = "018fb5b2-0000-7000-8000-000000000001" // p5b-claim-runner (P5b seed)

	s2aPlanM1Human  = "MST-WP-00601" // 一期里程碑图（peixun-m1）
	s2aPlanM23Human = "MST-WP-00602" // 二三期占位图（peixun-m23）

	s2aPatternName = "peixun-m1-bom"

	s2aJiraProjectKey = "PXPEIXUN"
	s2aGateID         = "bom"
	s2aBOMAssetID     = "ART-bom-001"

	// Item human codes occupy MST-WI-00701..00805 in manifest sort order.
	s2aItemCodeBase = 700
	// Flat work-item UUIDs 018fb5b4-0000-7000-8000-<idx>, idx 1..105.
	s2aFlatItemUUIDPrefix = "018fb5b4-0000-7000-8000-"
)

// Letter packages per plan: MST-WP-00611..00615 (M1 letters A..E),
// 00621..00625 (M23 letters), then numbered domain packages 00631+ (M1)
// and 00651+ (M23) in first-seen order.
const (
	s2aM1LetterCodeBase  = 611
	s2aM23LetterCodeBase = 621
	s2aM1DomainCodeBase  = 631
	s2aM23DomainCodeBase = 651
)

// ---------------------------------------------------------------------------
// Manifest shape (mirrors data/bom-manifest-20260909.json).
// ---------------------------------------------------------------------------

type bomManifestSource struct {
	File        string `json:"file"`
	SHA256      string `json:"sha256"`
	DocVersion  string `json:"doc_version"`
	Sheet       string `json:"sheet"`
	ExtractedAt string `json:"extracted_at"`
	Origin      string `json:"origin"`
}

type BomItem struct {
	Code     string `json:"code"`
	Domain   string `json:"domain"`
	Feature  string `json:"feature"`
	Desc     string `json:"desc"`
	Ref      string `json:"ref"`
	Impl     string `json:"impl"`
	Priority string `json:"priority"`
	Phase    string `json:"phase"`
	Days     int    `json:"days"`
}

type BomManifest struct {
	Source bomManifestSource `json:"source"`
	Items  []BomItem         `json:"items"`
}

// ---------------------------------------------------------------------------
// ID derivation (all pure, all stable across replays).
// ---------------------------------------------------------------------------

var (
	slotKeyLegal  = regexp.MustCompile(`^[a-z][a-z0-9.]{0,63}$`)
	localIDLegal  = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,63}$`)
	humanCodeForm = regexp.MustCompile(`^MST-(WP|WI)-[0-9]{5}$`)
	issueKeyLegal = regexp.MustCompile(`^[A-Z][A-Z0-9_]*-[0-9]+$`)
)

// letterOf returns the letter group A..E (the brief's intra-domain scope
// for requires edges: A1/A2/A3/A5/B/C/D/E).
func letterOf(code string) string { return code[:1] }

// domainKeyOf returns the numbered domain (A1, B10, …); the E letter
// carries no numbered domains — its items hang directly under the E
// package.
func domainKeyOf(code string) string {
	if letterOf(code) == "E" {
		return ""
	}
	for i := 1; i < len(code); i++ {
		if code[i] == '-' {
			return code[:i]
		}
	}
	return code
}

// itemSlotKey maps a BOM code to a legal node slot key: A1-1 → a1.1,
// E4 → e4.
func itemSlotKey(code string) string {
	out := []byte{}
	for i := range len(code) {
		c := code[i]
		if c >= 'A' && c <= 'Z' {
			c = c + ('a' - 'A')
		}
		if c == '-' {
			c = '.'
		}
		out = append(out, c)
	}
	return string(out)
}

// codeIndex maps each BOM code to its 1-based manifest position (the
// single ordering behind human codes, flat UUIDs and Jira issue keys).
func codeIndex(m BomManifest) map[string]int {
	index := map[string]int{}
	for i, item := range m.Items {
		index[item.Code] = i + 1
	}
	return index
}

// itemHumanCode: MST-WI-00701.. in manifest order.
func itemHumanCode(m BomManifest, code string) string {
	return fmt.Sprintf("MST-WI-%05d", s2aItemCodeBase+codeIndex(m)[code])
}

// flatItemUUID: the stable flat work-items substrate row for one BOM
// entry (anchors and gate bindings reference it).
func flatItemUUID(m BomManifest, code string) string {
	return fmt.Sprintf("%s%012d", s2aFlatItemUUIDPrefix, codeIndex(m)[code])
}

// jiraIssueKey derives the manual-anchor issue key (blueprint fallback,
// Jira unreachable): phase-1 stories PXPEIXUN-101.., phase-2/3
// placeholders PXPEIXUN-201.. (numbered across BOTH later phases so the
// series never collides).
func jiraIssueKey(m BomManifest, item BomItem) string {
	later := item.Phase != "一期"
	idx := 0
	for _, candidate := range m.Items {
		if (candidate.Phase != "一期") != later {
			continue
		}
		idx++
		if candidate.Code == item.Code {
			break
		}
	}
	base := 100
	if later {
		base = 200
	}
	return fmt.Sprintf("%s-%d", s2aJiraProjectKey, base+idx)
}

// flatPriority maps BOM priority onto the work_items CHECK vocabulary.
func flatPriority(bomPriority string) string {
	switch bomPriority {
	case "P0":
		return "high"
	case "P1":
		return "normal"
	default:
		return "low"
	}
}

// nodePriority maps BOM priority onto the NodeSpec numeric priority.
func nodePriority(bomPriority string) int {
	switch bomPriority {
	case "P0":
		return 1
	case "P1":
		return 2
	default:
		return 3
	}
}

// ---------------------------------------------------------------------------
// Intra-group requires edges (phase 1), derived from the BOM 说明 column.
// The full derivation table with quotes and the cross-group candidates
// lives in DERIVATION.md; this list is its machine form.
// ---------------------------------------------------------------------------

type bomEdge struct {
	From string // the item that requires
	To   string // the item being required
	Why  string
}

var m1RequiresEdges = []bomEdge{
	{"A1-6", "A1-1", "说明「按部门…组合圈选」——部门树为 A1-1 交付物"},
	{"A1-6", "A1-2", "说明「按…入职时间…圈选」——入职时间为 A1-2 档案字段"},
	{"A1-6", "A1-3", "说明「按…岗位…圈选」——岗位字典为 A1-3 交付物"},
	{"A1-4", "A1-1", "说明「数据权限（按部门树过滤）」——部门树为 A1-1 交付物"},
	{"A2-2", "A2-1", "章节（章—节—课时）隶属课程库条目——课程载体为 A2-1"},
	{"A2-9", "A2-1", "说明「音视频批量挂课」——挂课以课程库存在为前提"},
	{"A2-9", "A2-2", "说明「按文件生成课时」——课时为 A2-2 结构交付物"},
	{"A3-2", "A3-1", "说明「按人/课程/年度汇总学时」——学时来源为 A3-1 完成追踪"},
	{"A5-2", "A5-1", "题库条目携带题型/难度/知识点属性——题型目录为 A5-1 交付物"},
	{"A5-3", "A5-2", "批量导题写入题库——目标库为 A5-2 交付物"},
	{"B2-3", "B2-1", "说明「T-7/T-1/当天多轮提醒」——截止时点来自 B2-1 的完成标准与截止日期"},
	{"B2-3", "B8-1", "说明「多通道触达（站内/IM/短信）」——站内通道为 B8-1 交付物"},
	{"B5-2", "B5-1", "考试安排的场次以组卷产物（固定卷/随机卷）为用卷——B5-1 先行"},
	{"B5-4", "B5-2", "判分与复核作用于已安排场次的作答——B5-2 先行"},
	{"B5-6", "B5-4", "成绩报表统计判分产物（通过率/平均分/部门对比）——B5-4 先行"},
	{"B10-1", "B8-1", "说明「已读统计」——公告已读回执为 B8-1 交付物（B8-1 说明明写「公告已读回执」）"},
	{"C1-2", "C1-1", "说明「HLS+MP4 双协议」——HLS 切片为 C1-1「HLS 切片入点播库」产物"},
}

// ---------------------------------------------------------------------------
// Proposal derivation: one decomposition proposal per letter group.
// ---------------------------------------------------------------------------

// derivedNode is one proposal node in MCP wire form.
type derivedNode struct {
	LocalID   string         `json:"local_id"`
	Parent    string         `json:"parent"`
	NodeType  string         `json:"node_type"`
	SlotKey   string         `json:"slot_key"`
	HumanCode string         `json:"human_code"`
	Spec      map[string]any `json:"spec"`
}

type derivedEdge struct {
	From        string `json:"from"`
	To          string `json:"to"`
	Requirement string `json:"requirement"`
}

type derivedFlow struct {
	Node      string `json:"node"`
	Direction string `json:"direction"`
	AssetRef  string `json:"asset_ref"`
	PortKey   string `json:"port_key"`
}

// letterProposal is one letter group's batch for one plan.
type letterProposal struct {
	Letter string
	Nodes  []derivedNode
	Edges  []derivedEdge
	Flows  []derivedFlow
}

// proposalOptions carries the per-plan derivation inputs.
type proposalOptions struct {
	Phase          string // "一期" for M1, anything else for M23
	LetterCodeBase int
	DomainCodeBase int
	Repo           string // placeholder repo pin (S2B replans per item)
	BaselineSHA    string
	WorkspacePaths []string
	BOMAssetRef    string // ART-bom-001@<v> for M1 consume flows
	IncludeFlows   bool   // M1 only: placeholders stay flow-free
	IncludeEdges   bool   // M1 only: intra-group requires edges
}

// deriveLetterProposals splits one plan's items into per-letter
// proposals (node count ≤ 50 and fan-out ≤ 12 under the conservative
// defaults the MCP face enforces).
func deriveLetterProposals(m BomManifest, opt proposalOptions) []letterProposal {
	byLetter := map[string][]BomItem{}
	var letters []string
	for _, item := range m.Items {
		if (opt.Phase == "一期") != (item.Phase == "一期") {
			continue
		}
		letter := strings.ToLower(letterOf(item.Code))
		if _, seen := byLetter[letter]; !seen {
			letters = append(letters, letter)
		}
		byLetter[letter] = append(byLetter[letter], item)
	}
	sort.Strings(letters)

	// Numbered-domain human codes are ONE running series per plan
	// (letters sorted, domains sorted within each letter) so codes never
	// collide across letters.
	domainCodeOf := map[string]string{}
	{
		next := opt.DomainCodeBase
		for _, letter := range letters {
			var domains []string
			seen := map[string]bool{}
			for _, item := range byLetter[letter] {
				domain := domainKeyOf(item.Code)
				if domain == "" || seen[domain] {
					continue
				}
				seen[domain] = true
				domains = append(domains, domain)
			}
			sort.Strings(domains)
			for _, domain := range domains {
				domainCodeOf[domain] = fmt.Sprintf("MST-WP-%05d", next)
				next++
			}
		}
	}

	proposals := make([]letterProposal, 0, len(letters))
	for letterIndex, letter := range letters {
		items := byLetter[letter]
		group := letterProposal{Letter: letter}

		// Letter package under the plan root (parent filled by the caller
		// as ext:<root uuid>).
		group.Nodes = append(group.Nodes, derivedNode{
			LocalID:   "grp-" + letter,
			Parent:    "ROOT",
			NodeType:  "work_package",
			SlotKey:   letter,
			HumanCode: fmt.Sprintf("MST-WP-%05d", opt.LetterCodeBase+letterIndex),
			Spec: map[string]any{
				"title":               fmt.Sprintf("%s 域包（%s）", strings.ToUpper(letter), letterDomainTitle(m, strings.ToUpper(letter))),
				"acceptance_criteria": []string{"域内全部 WorkItem 达成（聚合证据随 S2B 逐条产生）"},
				"budget_units":        letterBudget(items),
				"success_threshold":   map[string]any{"kind": "all"},
				"failure_policy":      "needs_human",
				"cancel_policy":       "cascade_required",
			},
		})

		// Numbered domain packages under the letter (E skips this level).
		domainCodes := []string{}
		domainItems := map[string][]BomItem{}
		for _, item := range items {
			domain := domainKeyOf(item.Code)
			if domain == "" {
				continue
			}
			if _, seen := domainItems[domain]; !seen {
				domainCodes = append(domainCodes, domain)
			}
			domainItems[domain] = append(domainItems[domain], item)
		}
		sort.Strings(domainCodes)
		for _, domain := range domainCodes {
			group.Nodes = append(group.Nodes, derivedNode{
				LocalID:   "dom-" + strings.ToLower(domain),
				Parent:    "grp-" + letter,
				NodeType:  "work_package",
				SlotKey:   strings.ToLower(domain),
				HumanCode: domainCodeOf[domain],
				Spec: map[string]any{
					"title":               fmt.Sprintf("%s 域包（%s）", domain, domainItems[domain][0].Domain),
					"acceptance_criteria": []string{"域内全部 WorkItem 达成（聚合证据随 S2B 逐条产生）"},
					"budget_units":        letterBudget(domainItems[domain]),
					"success_threshold":   map[string]any{"kind": "all"},
					"failure_policy":      "needs_human",
					"cancel_policy":       "cascade_required",
				},
			})
		}

		// Item work nodes under their numbered domain (E: under grp-e).
		itemLocal := map[string]string{}
		for _, item := range items {
			parent := "grp-" + letter
			if domain := domainKeyOf(item.Code); domain != "" {
				parent = "dom-" + strings.ToLower(domain)
			}
			spec := map[string]any{
				"title":               fmt.Sprintf("[BOM %s] %s", item.Code, item.Feature),
				"repo":                opt.Repo,
				"baseline_sha":        opt.BaselineSHA,
				"workspace_paths":     opt.WorkspacePaths,
				"budget_units":        item.Days,
				"owning_capability":   "peixun." + itemDomainToken(item),
				"acceptance_criteria": []string{"S2B 详设 approved 且 locked_gate 生效", "实现 MR 人工合并且 Pipeline 绿"},
				"failure_policy":      "needs_human",
				"priority":            nodePriority(item.Priority),
			}
			local := strings.ToLower(item.Code)
			group.Nodes = append(group.Nodes, derivedNode{
				LocalID:   local,
				Parent:    parent,
				NodeType:  "work_item",
				SlotKey:   itemSlotKey(item.Code),
				HumanCode: itemHumanCode(m, item.Code),
				Spec:      spec,
			})
			itemLocal[item.Code] = local
		}

		if opt.IncludeEdges {
			for _, edge := range m1RequiresEdges {
				from, fromOK := itemLocal[edge.From]
				to, toOK := itemLocal[edge.To]
				if !fromOK || !toOK {
					continue // edge endpoints outside this letter/group
				}
				group.Edges = append(group.Edges, derivedEdge{From: from, To: to, Requirement: "required"})
			}
		}
		if opt.IncludeFlows {
			for _, item := range items {
				group.Flows = append(group.Flows, derivedFlow{
					Node: strings.ToLower(item.Code), Direction: "consumes",
					AssetRef: opt.BOMAssetRef, PortKey: "bom.baseline",
				})
			}
		}
		proposals = append(proposals, group)
	}
	return proposals
}

// letterBudget: a package's budget equals its descendant item budget
// sum (the validator's envelope check is sum ≤ package).
func letterBudget(items []BomItem) int {
	total := 0
	for _, item := range items {
		total += item.Days
	}
	return total
}

// itemDomainToken: the owning-capability token derived from the item's
// numbered domain (a1, b10, e …).
func itemDomainToken(item BomItem) string {
	if domain := domainKeyOf(item.Code); domain != "" {
		return itemSlotKey(domain)
	}
	return letterOf(item.Code)
}

// letterDomainTitle: the human domain name of a letter (from its first
// item's 功能域 prefix before the ·).
func letterDomainTitle(m BomManifest, letter string) string {
	for _, item := range m.Items {
		if letterOf(item.Code) == letter {
			for i := range len(item.Domain) {
				if item.Domain[i] == '·' {
					return item.Domain[:i]
				}
			}
			return item.Domain
		}
	}
	return letter
}

// ---------------------------------------------------------------------------
// Structural self-checks (run before touching the live surfaces; the
// unit tests assert the same properties).
// ---------------------------------------------------------------------------

func checkDerived(proposals []letterProposal) error {
	nodesByParent := map[string]int{}
	humanCodes := map[string]bool{}
	slots := map[string]bool{}
	for _, group := range proposals {
		if len(group.Nodes) > 50 {
			return fmt.Errorf("letter %s proposal carries %d nodes (limit 50)", group.Letter, len(group.Nodes))
		}
		for _, node := range group.Nodes {
			if !localIDLegal.MatchString(node.LocalID) {
				return fmt.Errorf("local id %q illegal", node.LocalID)
			}
			if !slotKeyLegal.MatchString(node.SlotKey) {
				return fmt.Errorf("slot key %q illegal", node.SlotKey)
			}
			if !humanCodeForm.MatchString(node.HumanCode) {
				return fmt.Errorf("human code %q illegal", node.HumanCode)
			}
			if humanCodes[node.HumanCode] {
				return fmt.Errorf("human code %q duplicated", node.HumanCode)
			}
			humanCodes[node.HumanCode] = true
			key := node.Parent + "\x00" + node.SlotKey
			if slots[key] {
				return fmt.Errorf("slot %q duplicated under %s", node.SlotKey, node.Parent)
			}
			slots[key] = true
			if node.Parent != "ROOT" {
				nodesByParent[node.Parent]++
			}
		}
	}
	for parent, count := range nodesByParent {
		if parent != "ROOT" && count > 12 {
			return fmt.Errorf("parent %s carries %d children (limit 12)", parent, count)
		}
	}
	// Edge endpoints must exist in the same proposal batch and stay
	// acyclic (letter groups are small; DFS per group).
	for _, group := range proposals {
		present := map[string]bool{}
		for _, node := range group.Nodes {
			present[node.LocalID] = true
		}
		adj := map[string][]string{}
		for _, edge := range group.Edges {
			if !present[edge.From] || !present[edge.To] {
				return fmt.Errorf("edge %s→%s endpoint missing in letter %s", edge.From, edge.To, group.Letter)
			}
			adj[edge.From] = append(adj[edge.From], edge.To)
		}
		visit := map[string]int{}
		var walk func(string) bool
		walk = func(node string) bool {
			visit[node] = 1
			for _, next := range adj[node] {
				switch visit[next] {
				case 1:
					return true
				case 0:
					if walk(next) {
						return true
					}
				}
			}
			visit[node] = 2
			return false
		}
		for node := range adj {
			if visit[node] == 0 && walk(node) {
				return fmt.Errorf("requires cycle in letter %s", group.Letter)
			}
		}
	}
	return nil
}
