# Specification Quality Checklist: Stage 2 Schema & Mount Alignment (`010-schema-mount-alignment`)

- [x] User stories cover `sandbox.entrypoint` removal, `qc.sandbox`/`qc.profile` overrides, and `/.sofa/{artifacts,imports}` path alignment
- [x] `golden.Decode` native rejection of `entrypoint` and DAG edge creation for `qc.sandbox` are preserved without re-implementing `golden`
- [x] Intra-set path overlap rules (`exact` and `parent/child`) vs cross-set isolation (`/.sofa/artifacts` vs `/.sofa/imports`) are specified
- [x] `ExportPlan` schema changes (`sandbox`, `qc`, `artifacts`, `imports`) and `DeferredExpression` alignment are specified
- [x] Out-of-scope Stage 2 items (Podman runtime, `BlobStore`, Git adapter, harness plugin protocol) are excluded
