# ECS adapter configuration lifetime

`ecs.New` snapshots every mutable level of its trusted operator configuration:
targets, task-definition mappings, predecessor lists, expected container digests
and rollback container digests. Editing the original maps or slices after
construction cannot redirect writes or change runtime expectations. Construct a
new validated adapter to apply a configuration update; do not mutate an active
adapter from another goroutine. Constructor input must not be concurrently
modified during construction.

This changes no request format or AWS calls. It makes the existing local
allowlist lifetime explicit. Tests exercise deploy and rollback after edits to
all caller-owned maps/slices and confirm unmapped digests still cause no writes.
Actual AWS staging remains a separate #7/#11 acceptance gate.
