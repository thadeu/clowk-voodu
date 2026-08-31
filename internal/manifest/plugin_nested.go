package manifest

import (
	"fmt"

	"github.com/hashicorp/hcl/v2/gohcl"
	"github.com/hashicorp/hcl/v2/hclsyntax"
)

// extractPluginBlocks splits a core kind's body into the blocks its own
// schema knows and the blocks it does not.
//
// The unknown ones are not an error. A block the core does not
// recognise inside a deployment belongs to a plugin — `traffik {}` and
// whatever comes after it — and rejecting it here would mean every
// plugin that wants a nested block has to land a change in the core
// parser before it can ship.
//
// What the core keeps is the block's name and body, verbatim. It never
// looks inside Spec. That is the line that stops a nested plugin block
// from becoming a core contract: the parser learns "this workload has a
// block belonging to plugin X", and nothing about what X does with it.
//
// The known set comes from gohcl's own schema for the target struct
// rather than a hand-kept list, so adding a block to hclDeployment
// cannot silently start routing it to a plugin.
//
// A typo still surfaces, just later and better: `probs {}` is carried
// here, finds no plugin named "probs" at apply, and fails with a
// message that can name the block it resembles — where gohcl could only
// say the block was unexpected.
func extractPluginBlocks(body *hclsyntax.Body, schemaOf any) (*hclsyntax.Body, []PluginBlock, error) {
	schema, _ := gohcl.ImpliedBodySchema(schemaOf)

	known := make(map[string]bool, len(schema.Blocks))

	for _, b := range schema.Blocks {
		known[b.Type] = true
	}

	var (
		keep    hclsyntax.Blocks
		plugins []PluginBlock
	)

	for _, blk := range body.Blocks {
		if known[blk.Type] {
			keep = append(keep, blk)

			continue
		}

		spec, err := bodyToJSON(blk.Body)

		if err != nil {
			return nil, nil, fmt.Errorf("%s block: %w", blk.Type, err)
		}

		plugins = append(plugins, PluginBlock{
			Type:   blk.Type,
			Labels: append([]string(nil), blk.Labels...),
			Spec:   spec,
		})
	}

	// Nothing unknown: hand back the original body untouched, so the
	// overwhelmingly common manifest takes no copy and no allocation.
	if len(plugins) == 0 {
		return body, nil, nil
	}

	// gohcl is handed a body carrying only the blocks it declares, so
	// its strictness is preserved for everything it does own.
	return &hclsyntax.Body{
		Attributes: body.Attributes,
		Blocks:     keep,
		SrcRange:   body.SrcRange,
		EndRange:   body.EndRange,
	}, plugins, nil
}
