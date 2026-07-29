DROP TRIGGER IF EXISTS trg_feynman_evaluation_decisions_append_only ON feynman_evaluation_decisions;
DROP FUNCTION IF EXISTS reject_feynman_evaluation_decision_mutation();
DROP TABLE IF EXISTS feynman_evaluation_decisions;
DROP TRIGGER IF EXISTS trg_feynman_evaluations_guard ON feynman_evaluations;
DROP FUNCTION IF EXISTS guard_feynman_evaluation_transition();
DROP TABLE IF EXISTS feynman_evaluations;