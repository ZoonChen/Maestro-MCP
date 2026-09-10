-- Reverse of 0017 (expand step one only; no live data exists on these
-- tables before cutover, so a plain drop set is the honest rollback).

DROP TABLE IF EXISTS asset_gate_bindings;
DROP TABLE IF EXISTS node_artifact_flows;
DROP TRIGGER IF EXISTS trg_assets_no_delete ON assets;
DROP TRIGGER IF EXISTS trg_assets_status_transition ON assets;
DROP TRIGGER IF EXISTS trg_assets_content_immutable ON assets;
DROP FUNCTION IF EXISTS maestro_asset_status_transition();
DROP FUNCTION IF EXISTS maestro_asset_content_immutable();
DROP TABLE IF EXISTS assets;

ALTER TABLE work_plans DROP CONSTRAINT IF EXISTS fk_work_plans_root;
DROP TRIGGER IF EXISTS trg_work_plans_root_immutable ON work_plans;
DROP FUNCTION IF EXISTS maestro_work_plan_root_immutable();
DROP TABLE IF EXISTS node_lineage;
DROP TABLE IF EXISTS work_dependencies;
DROP TRIGGER IF EXISTS trg_work_node_revisions_immutable ON work_node_revisions;
DROP TABLE IF EXISTS work_node_revisions;
DROP TRIGGER IF EXISTS trg_plan_revisions_sealed_immutable ON plan_revisions;
DROP FUNCTION IF EXISTS maestro_plan_revision_sealed_immutable();
DROP TABLE IF EXISTS plan_revisions;
DROP TRIGGER IF EXISTS trg_work_nodes_structure_immutable ON work_nodes;
DROP FUNCTION IF EXISTS maestro_work_node_structure_immutable();
DROP TABLE IF EXISTS work_nodes;
DROP TABLE IF EXISTS work_plans;
