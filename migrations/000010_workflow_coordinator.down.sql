DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM workflow_schedule_reservations
        WHERE status IN ('RESERVED', 'ADMITTED', 'UNKNOWN')
    ) THEN
        RAISE EXCEPTION 'cannot remove workflow coordinator tables while reservations are unresolved';
    END IF;
END
$$;

DROP TABLE workflow_schedule_outcomes;
DROP TABLE workflow_schedule_admissions;
DROP TABLE workflow_schedule_scopes;
DROP TABLE workflow_schedule_counters;
DROP TABLE workflow_circuit_breakers;
DROP TABLE workflow_schedule_reservations;
