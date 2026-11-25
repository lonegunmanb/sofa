# Contract: Planning Functions and Context

All functions are available to expressions evaluated during offline planning. They never perform network calls or start external processes.

## `cwd`

Signature:

```text
cwd() -> string
```

Returns the absolute current working directory at evaluation time with separators normalized to `/`.

## `one`

Signature:

```text
one(collection) -> element | null
```

Rules:

- `null` is an error.
- A non-collection is an error.
- An empty known collection returns `cty.NullVal(elementType)`.
- A singleton known collection returns its only element.
- A collection with more than one element is an error.
- An unknown collection returns an unknown value with the collection's element type where that type is determinate.

## `local_timestamp`

Signature:

```text
local_timestamp() -> string
```

Returns `time.Now()` formatted with RFC3339Nano.

## `jsondecodewithtype`

Signature:

```text
jsondecodewithtype(json_string, type_expression) -> typed value
```

Rules:

- The first argument MUST be a string.
- The second argument MUST parse as a type constraint.
- Known JSON is decoded and converted to the exact declared type.
- Invalid JSON, JSON/type incompatibility, or an invalid type constraint is an error.
- An unknown JSON string with a known type expression returns an unknown value of exactly that type.

Example:

```hcl
jsondecodewithtype(
  "{\"checks\":{\"a\":\"b\"}}",
  object({checks = map(string)})
)
```

Result type:

```text
object({checks = map(string)})
```

## Evaluation context

```hcl
path.module
```

Returns the absolute directory of the currently planned module with `/` separators.

The context also retains every function supplied by the upstream function library.
