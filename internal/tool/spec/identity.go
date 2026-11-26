package spec

import (
	"crypto/sha256"
	"fmt"
	"maps"
	"strings"

	"github.com/lonegunmanb/golden"
	"github.com/zclconf/go-cty/cty"
)

const maxSDKToolNameLength = 64

type canonicalAddressProvider interface {
	CanonicalAddress(string) string
}

func canonicalAddress(block *golden.BaseBlock) string {
	if block != nil && block.Config() != nil {
		if provider, ok := block.Config().(canonicalAddressProvider); ok {
			return provider.CanonicalAddress(block.Address())
		}
	}
	return block.Address()
}

func typedToolID(block *golden.BaseBlock, blockType string) string {
	canonical := canonicalAddress(block)
	digest := sha256.Sum256([]byte("sofa/tool-id/v1\x00" + blockType + "\x00" + canonical))
	digest[6] = (digest[6] & 0x0f) | 0x80
	digest[8] = (digest[8] & 0x3f) | 0x80
	uuid := fmt.Sprintf(
		"%x-%x-%x-%x-%x",
		digest[0:4], digest[4:6], digest[6:8], digest[8:10], digest[10:16],
	)
	nameLength := max(0, maxSDKToolNameLength-len("tool_")-len(blockType)-len("__")-len(uuid))
	name := sdkNamePart(block.Name(), nameLength)
	return "tool_" + blockType + "_" + name + "_" + uuid
}

func sdkNamePart(name string, limit int) string {
	var result strings.Builder
	for _, character := range name {
		if result.Len() >= limit {
			break
		}
		switch {
		case character >= 'a' && character <= 'z',
			character >= 'A' && character <= 'Z',
			character >= '0' && character <= '9',
			character == '_', character == '-':
			result.WriteRune(character)
		default:
			result.WriteByte('_')
		}
	}
	if result.Len() == 0 {
		fallback := "tool"
		if limit < len(fallback) {
			return fallback[:limit]
		}
		return fallback
	}
	return result.String()
}

func typedToolBaseValues(block *golden.BaseBlock, id string) map[string]cty.Value {
	values := maps.Clone(block.BaseValues())
	if values == nil {
		values = make(map[string]cty.Value, 1)
	}
	values["id"] = cty.StringVal(id)
	return values
}
