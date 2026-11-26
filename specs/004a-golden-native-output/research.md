# Phase 0 Research

## Decision: Pin golden `059cc8c`

Latest module metadata resolves to `v0.0.0-20261125162047-059cc8cb0d1d`. Source inspection confirms built-in `OutputBlock`, common-block registration, single-label adaptation, expression text, and sensitivity accessors.

## Decision: Keep only a pure snapshot DTO

Planner and later plan serialization need stable data without mutable block ownership. The DTO contains name, value, description, sensitivity, and original expression.

## Decision: Reuse existing regression tests

Existing tests cover known, unknown, null, invalid schema, sensitivity propagation, and cold start; they now validate the native block.

No unresolved questions.
