// Package eval implements the M4-EVAL-001 record core: validation of
// the four-layer evaluation record against the frozen
// evaluation-record.schema.json (every wire constraint mirrored
// here, including the two conditional requirements — security
// records require risk, failed records require notes) and the pass^k
// estimator over repeated trials. Persistence lives in internal/store.
package eval

import (
	"errors"
	"fmt"
	"regexp"
	"unicode/utf8"

	"github.com/google/uuid"
)

// SchemaVersion is the frozen wire version of the record.
const SchemaVersion = "3.0"

// Layer is the four-layer vocabulary.
type Layer string

const (
	LayerQuality    Layer = "quality"
	LayerTrajectory Layer = "trajectory"
	LayerSecurity   Layer = "security"
	LayerCapability Layer = "capability"
)

var layers = map[Layer]bool{
	LayerQuality: true, LayerTrajectory: true, LayerSecurity: true, LayerCapability: true,
}

// Verdict is the frozen outcome vocabulary.
type Verdict string

const (
	VerdictPass    Verdict = "pass"
	VerdictFail    Verdict = "fail"
	VerdictError   Verdict = "error"
	VerdictBlocked Verdict = "blocked"
	VerdictSkipped Verdict = "skipped"
)

var verdicts = map[Verdict]bool{
	VerdictPass: true, VerdictFail: true, VerdictError: true,
	VerdictBlocked: true, VerdictSkipped: true,
}

// Risk is the frozen security-risk vocabulary.
type Risk string

const (
	RiskBenign   Risk = "benign"
	RiskLow      Risk = "low"
	RiskElevated Risk = "elevated"
	RiskHigh     Risk = "high"
)

var risks = map[Risk]bool{
	RiskBenign: true, RiskLow: true, RiskElevated: true, RiskHigh: true,
}

// ScorerKind is the frozen scorer vocabulary.
type ScorerKind string

const (
	ScorerDeterministic ScorerKind = "deterministic"
	ScorerRuleBased     ScorerKind = "rule_based"
	ScorerLLMJudge      ScorerKind = "llm_judge"
	ScorerHuman         ScorerKind = "human"
)

var scorerKinds = map[ScorerKind]bool{
	ScorerDeterministic: true, ScorerRuleBased: true, ScorerLLMJudge: true, ScorerHuman: true,
}

// Sentinels.
var (
	ErrRecordRejected = errors.New("eval: record rejected")
	ErrPassAtKInvalid = errors.New("eval: pass^k arguments invalid")
)

var sha256DigestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// Scorer identifies the scoring authority; an LLM judge must carry
// its calibration reference (enforced on the wire by callers, kept
// here as a validated field set).
type Scorer struct {
	Kind           ScorerKind
	Version        string
	CalibrationRef string
}

// Record is one four-layer evaluation record (the wire shape).
type Record struct {
	SchemaVersion            string
	RecordID                 string
	ProjectID                string
	Layer                    Layer
	CaseID                   string
	RunID                    string
	Category                 string
	Risk                     Risk
	Verdict                  Verdict
	Score                    float64
	Scorer                   Scorer
	DatasetDigest            string
	Seed                     *int64
	ModelConfigDigest        string
	TrajectoryConstraints    []string
	ForbiddenActionsObserved []string
	TokensUsed               int64
	LatencyMS                *int64
	ExternalStateDigest      string
	EvidenceRefs             []string
	Notes                    string
	Version                  int64
}

// Validate enforces the frozen wire contract. The two conditional
// requirements are fail-closed: a security record without risk and a
// failed record without notes are rejected before anything persists.
func (r Record) Validate() error {
	if r.SchemaVersion != "" && r.SchemaVersion != SchemaVersion {
		return fmt.Errorf("%w: schema_version must be %q", ErrRecordRejected, SchemaVersion)
	}
	if r.RecordID != "" {
		if _, err := uuid.Parse(r.RecordID); err != nil {
			return fmt.Errorf("%w: record_id is not a uuid", ErrRecordRejected)
		}
	}
	if !layers[r.Layer] {
		return fmt.Errorf("%w: layer %q not in the frozen set", ErrRecordRejected, r.Layer)
	}
	if length := utf8.RuneCountInString(r.CaseID); length < 1 || length > 128 {
		return fmt.Errorf("%w: case_id length %d outside 1..128", ErrRecordRejected, length)
	}
	if _, err := uuid.Parse(r.RunID); err != nil {
		return fmt.Errorf("%w: run_id is not a uuid", ErrRecordRejected)
	}
	if r.Category != "" && (utf8.RuneCountInString(r.Category) < 1 || utf8.RuneCountInString(r.Category) > 128) {
		return fmt.Errorf("%w: category length outside 1..128", ErrRecordRejected)
	}
	if r.Layer == LayerSecurity && !risks[r.Risk] {
		return fmt.Errorf("%w: security records require a frozen risk value, got %q", ErrRecordRejected, r.Risk)
	}
	if r.Risk != "" && !risks[r.Risk] {
		return fmt.Errorf("%w: risk %q not in the frozen set", ErrRecordRejected, r.Risk)
	}
	if !verdicts[r.Verdict] {
		return fmt.Errorf("%w: verdict %q not in the frozen set", ErrRecordRejected, r.Verdict)
	}
	if r.Score < 0 || r.Score > 100 {
		return fmt.Errorf("%w: score %v outside 0..100", ErrRecordRejected, r.Score)
	}
	if !scorerKinds[r.Scorer.Kind] {
		return fmt.Errorf("%w: scorer kind %q not in the frozen set", ErrRecordRejected, r.Scorer.Kind)
	}
	if length := utf8.RuneCountInString(r.Scorer.Version); length < 1 || length > 64 {
		return fmt.Errorf("%w: scorer version length %d outside 1..64", ErrRecordRejected, length)
	}
	if r.Scorer.CalibrationRef != "" && (utf8.RuneCountInString(r.Scorer.CalibrationRef) < 1 || utf8.RuneCountInString(r.Scorer.CalibrationRef) > 255) {
		return fmt.Errorf("%w: calibration ref length outside 1..255", ErrRecordRejected)
	}
	if !sha256DigestPattern.MatchString(r.DatasetDigest) {
		return fmt.Errorf("%w: dataset_digest must be sha256:<64 hex>", ErrRecordRejected)
	}
	if r.Seed != nil && *r.Seed < 0 {
		return fmt.Errorf("%w: negative seed", ErrRecordRejected)
	}
	if r.ModelConfigDigest != "" && !sha256DigestPattern.MatchString(r.ModelConfigDigest) {
		return fmt.Errorf("%w: model_config_digest must be sha256:<64 hex>", ErrRecordRejected)
	}
	if err := validateUniqueBounded("trajectory constraint", r.TrajectoryConstraints, 200); err != nil {
		return err
	}
	if err := validateUniqueBounded("forbidden action", r.ForbiddenActionsObserved, 200); err != nil {
		return err
	}
	if r.TokensUsed < 0 {
		return fmt.Errorf("%w: negative token usage", ErrRecordRejected)
	}
	if r.LatencyMS != nil && *r.LatencyMS < 0 {
		return fmt.Errorf("%w: negative latency", ErrRecordRejected)
	}
	if r.ExternalStateDigest != "" && !sha256DigestPattern.MatchString(r.ExternalStateDigest) {
		return fmt.Errorf("%w: external_state_digest must be sha256:<64 hex>", ErrRecordRejected)
	}
	if err := validateUniqueUUIDs("evidence ref", r.EvidenceRefs); err != nil {
		return err
	}
	if utf8.RuneCountInString(r.Notes) > 4000 {
		return fmt.Errorf("%w: notes exceed 4000 characters", ErrRecordRejected)
	}
	if r.Verdict == VerdictFail && r.Notes == "" {
		return fmt.Errorf("%w: failed records require notes", ErrRecordRejected)
	}
	if r.Version < 1 {
		return fmt.Errorf("%w: version must be >= 1", ErrRecordRejected)
	}
	return nil
}

func validateUniqueBounded(what string, values []string, maxLength int) error {
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		length := utf8.RuneCountInString(value)
		if length < 1 || length > maxLength {
			return fmt.Errorf("%w: %s length %d outside 1..%d", ErrRecordRejected, what, length, maxLength)
		}
		if seen[value] {
			return fmt.Errorf("%w: duplicate %s %q", ErrRecordRejected, what, value)
		}
		seen[value] = true
	}
	return nil
}

func validateUniqueUUIDs(what string, values []string) error {
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		if _, err := uuid.Parse(value); err != nil {
			return fmt.Errorf("%w: %s %q is not a uuid", ErrRecordRejected, what, value)
		}
		if seen[value] {
			return fmt.Errorf("%w: duplicate %s %q", ErrRecordRejected, what, value)
		}
		seen[value] = true
	}
	return nil
}

// PassPowerK estimates pass^k — the probability that k trials chosen
// uniformly at random from n repeated trials ALL pass,
// C(passes, k)/C(n, k) in the product form so large n cannot
// overflow. pass^1 is the observed pass rate; pass^k decreases as k
// grows (the authority's key-scenario pass^3 is this at k=3). k must
// lie in 1..n; passes below k yields 0, never a negative probability.
func PassPowerK(n, passes, k int) (float64, error) {
	if n < 1 || passes < 0 || passes > n || k < 1 || k > n {
		return 0, fmt.Errorf("%w: n=%d passes=%d k=%d", ErrPassAtKInvalid, n, passes, k)
	}
	result := 1.0
	for i := range k {
		if passes-i <= 0 {
			return 0, nil
		}
		result *= float64(passes-i) / float64(n-i)
	}
	return result, nil
}
