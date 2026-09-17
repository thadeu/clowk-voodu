// Package manifestrefs reads bucket references off an HCL manifest's syntax
// tree — before the manifest is parsed, which is the only time the answer is
// useful, since `${VAR}` interpolation runs first. A leaf package on purpose:
// both cmd/cli and internal/controller need it, and internal/manifest already
// imports the controller.
package manifestrefs

import (
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
)

// EnvFromRefs lists the `env_from = [...]` bucket refs an HCL manifest
// declares, in order of first appearance, without evaluating anything.
//
// Read off the syntax tree rather than the parsed manifest because the
// reason to want them is to feed `${VAR}` interpolation, which happens
// BEFORE the manifest can be parsed. Both the CLI (`vd apply`, against the
// operator's shell) and the controller (a GitHub push, with no shell at all)
// use this to decide which buckets to load first.
func EnvFromRefs(filename string, raw []byte) []string {
	body := syntaxBody(filename, raw)
	if body == nil {
		return nil
	}

	seen := map[string]struct{}{}

	var refs []string

	for _, blk := range body.Blocks {
		if blk.Body == nil {
			continue
		}

		for _, attr := range blk.Body.Attributes {
			if attr.Name != "env_from" {
				continue
			}

			tuple, ok := attr.Expr.(*hclsyntax.TupleConsExpr)
			if !ok {
				continue
			}

			for _, expr := range tuple.Exprs {
				s, ok := literalString(expr)
				if !ok || s == "" {
					continue
				}

				if _, dup := seen[s]; dup {
					continue
				}

				seen[s] = struct{}{}
				refs = append(refs, s)
			}
		}
	}

	return refs
}

// ResourceRefs lists every `<scope>/<name>` the manifest's two-label blocks
// name — the resources' own buckets, which `${VAR}` may draw on even with no
// env_from declared.
func ResourceRefs(filename string, raw []byte) []string {
	body := syntaxBody(filename, raw)
	if body == nil {
		return nil
	}

	seen := map[string]struct{}{}

	var refs []string

	for _, blk := range body.Blocks {
		if len(blk.Labels) != 2 {
			continue
		}

		scope, name := blk.Labels[0], blk.Labels[1]
		if scope == "" || name == "" {
			continue
		}

		ref := scope + "/" + name
		if _, dup := seen[ref]; dup {
			continue
		}

		seen[ref] = struct{}{}
		refs = append(refs, ref)
	}

	return refs
}

func syntaxBody(filename string, raw []byte) *hclsyntax.Body {
	file, _ := hclsyntax.ParseConfig(raw, filename, hcl.Pos{Line: 1, Column: 1})
	if file == nil {
		return nil
	}

	body, ok := file.Body.(*hclsyntax.Body)
	if !ok {
		return nil
	}

	return body
}

func literalString(expr hclsyntax.Expression) (string, bool) {
	tmpl, ok := expr.(*hclsyntax.TemplateExpr)
	if !ok || len(tmpl.Parts) != 1 {
		return "", false
	}

	lit, ok := tmpl.Parts[0].(*hclsyntax.LiteralValueExpr)
	if !ok || lit.Val.Type().FriendlyName() != "string" {
		return "", false
	}

	return lit.Val.AsString(), true
}
