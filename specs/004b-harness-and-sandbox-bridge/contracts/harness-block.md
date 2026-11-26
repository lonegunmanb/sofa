# Harness Block Contract

## HCL Schema (`harness "<name>"`)

```hcl
harness "<name>" {
  source      = "<host>/<owner>/<repo>" # optional; paired with version
  version     = "0.1.0"                 # optional; exact SemVer without "v"
  signature   = "auto"                  # optional; "auto" (default) | "attestation" | "pgp" | "none"
  signing_key = null                    # optional; required if signature == "pgp"

  env_refs = {                          # optional; map(string), env var name -> host env var name
    KEY = "HOST_KEY"
  }

  options = {                           # optional; wholly known object/map
    key = "value"
  }
}
```

- Unknown attributes (including `model_provider`) and unknown nested blocks are rejected by `golden.Decode`.
- `golden` meta-attributes (`for_each`, `depends_on`) and meta-blocks (`precondition`, `dynamic`) are natively supported.

## Go API (`internal/harness/spec`)

```go
type SignatureMode string

const (
	SignatureModeAuto        SignatureMode = "auto"
	SignatureModeAttestation SignatureMode = "attestation"
	SignatureModePGP         SignatureMode = "pgp"
	SignatureModeNone        SignatureMode = "none"
)

type HarnessConfig struct {
	Name        string
	Source      string
	SourceHost  string
	SourceOwner string
	SourceRepo  string
	Version     string
	Signature   SignatureMode
	SigningKey  string
	EnvRefs     map[string]string
	Options     cty.Value
}

func (c *HarnessConfig) Validate() error
func (c HarnessConfig) Clone() HarnessConfig

type HarnessBlock struct { ... }
func (b *HarnessBlock) HarnessConfig() HarnessConfig
func (b *HarnessBlock) Value() cty.Value
```

- `HarnessBlock` implements `golden.PlanBlock`, `golden.SingleValueBlock`, and `golden.BaseDecode`.
- `Type() == ""`, `BlockType() == "harness"`, `AddressLength() == 2`, `CanExecutePrePlan() == false`.
- No network requests, no plugin process spawning, no host environment variable reading during Plan.
