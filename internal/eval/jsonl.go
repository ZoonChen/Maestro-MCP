// JSONL parsing helper for the ingest path (M4-EVAL-001 wiring): one
// frozen-wire record per line, as emitted by tests/eval. This adds no
// validation semantics — Record.Validate stays the single authority;
// parsing only mirrors the schema's closed property set
// (additionalProperties: false) and the const schema_version.
package eval

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

type wireScorer struct {
	Kind           string `json:"kind"`
	Version        string `json:"version"`
	CalibrationRef string `json:"calibration_ref"`
}

// wireRecord mirrors evaluation-record.schema.json exactly; created_at
// is accepted but not mapped (the store stamps its own timestamp).
type wireRecord struct {
	SchemaVersion            string     `json:"schema_version"`
	RecordID                 string     `json:"record_id"`
	Layer                    Layer      `json:"layer"`
	CaseID                   string     `json:"case_id"`
	RunID                    string     `json:"run_id"`
	Category                 string     `json:"category"`
	Risk                     Risk       `json:"risk"`
	Verdict                  Verdict    `json:"verdict"`
	Score                    float64    `json:"score"`
	Scorer                   wireScorer `json:"scorer"`
	DatasetDigest            string     `json:"dataset_digest"`
	Seed                     *int64     `json:"seed"`
	ModelConfigDigest        string     `json:"model_config_digest"`
	TrajectoryConstraints    []string   `json:"trajectory_constraints"`
	ForbiddenActionsObserved []string   `json:"forbidden_actions_observed"`
	TokensUsed               int64      `json:"tokens_used"`
	LatencyMS                *int64     `json:"latency_ms"`
	ExternalStateDigest      string     `json:"external_state_digest"`
	EvidenceRefs             []string   `json:"evidence_refs"`
	Notes                    string     `json:"notes"`
	Version                  int64      `json:"version"`
	CreatedAt                string     `json:"created_at"`
}

// ParseRecord decodes one JSONL line into a Record. The frozen wire
// requires schema_version to be exactly "3.0" and allows no unknown
// properties or trailing content; every other rule (required fields,
// vocabularies, digests, the two conditional requirements) stays with
// Record.Validate.
func ParseRecord(line []byte) (Record, error) {
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.DisallowUnknownFields()
	var wire wireRecord
	if err := decoder.Decode(&wire); err != nil {
		return Record{}, fmt.Errorf("eval: jsonl line is not a record object: %w", err)
	}
	if err := decoder.Decode(new(json.RawMessage)); err != io.EOF {
		return Record{}, fmt.Errorf("eval: jsonl line has trailing content after the record object")
	}
	if wire.SchemaVersion != SchemaVersion {
		return Record{}, fmt.Errorf("eval: jsonl schema_version must be %q, got %q", SchemaVersion, wire.SchemaVersion)
	}
	return Record{
		SchemaVersion:            wire.SchemaVersion,
		RecordID:                 wire.RecordID,
		Layer:                    wire.Layer,
		CaseID:                   wire.CaseID,
		RunID:                    wire.RunID,
		Category:                 wire.Category,
		Risk:                     wire.Risk,
		Verdict:                  wire.Verdict,
		Score:                    wire.Score,
		Scorer:                   Scorer{Kind: ScorerKind(wire.Scorer.Kind), Version: wire.Scorer.Version, CalibrationRef: wire.Scorer.CalibrationRef},
		DatasetDigest:            wire.DatasetDigest,
		Seed:                     wire.Seed,
		ModelConfigDigest:        wire.ModelConfigDigest,
		TrajectoryConstraints:    wire.TrajectoryConstraints,
		ForbiddenActionsObserved: wire.ForbiddenActionsObserved,
		TokensUsed:               wire.TokensUsed,
		LatencyMS:                wire.LatencyMS,
		ExternalStateDigest:      wire.ExternalStateDigest,
		EvidenceRefs:             wire.EvidenceRefs,
		Notes:                    wire.Notes,
		Version:                  wire.Version,
	}, nil
}
