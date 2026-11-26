# Native Output Contract

- Input syntax remains `output "<name>" { value = ... }`.
- Golden owns decoding, dynamic/precondition handling, validation, value evaluation, unknown retention, and sensitivity.
- Planner snapshots use only public `golden.OutputBlock` accessors.
- Sofa must not register an output block.
