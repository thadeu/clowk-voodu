package manifest

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/function"
)

// assetEvalContext returns the HCL EvalContext used while
// decoding an `asset { … }` block body. Two functions are
// exposed:
//
//   - file(path)  — read the file at `path` (relative to the
//                   manifest file's directory; absolute paths
//                   honoured as-is). Returns the typed object
//                   the controller materialises as `{_source:
//                   "file", content:"<base64>", filename:
//                   "<basename>"}`. CLI reads at parse time so
//                   the server gets the bytes embedded in the
//                   manifest JSON.
//
//   - url(href)   — declare a remote resource the server fetches
//                   server-side at materialisation time.
//                   Returns `{_source:"url", url:"…"}`. URL
//                   sources are cached server-side by
//                   ETag/Last-Modified to keep re-applies fast.
//
// Inline string literals (`key = "value"`) are NOT wrapped —
// they pass straight through ctyValueToGo as Go strings, and
// the server treats any plain string as an inline source
// without further inspection.
//
// The `_source` discriminator is reserved: operators must not
// declare a key with that name themselves at the asset level
// (today the parser doesn't enforce this, but the server-side
// materialiser would interpret it as a source object). Future
// validation can lock it down.
//
// `manifestPath` is the disk path of the .hcl file being
// parsed. Used to resolve `file("./...")` relative paths so
// `vd apply -f infra/db/voodu.hcl` finds files declared as
// `file("./configs/redis.conf")` in the manifest's directory,
// not in the CLI's CWD. Empty string falls back to CWD-relative
// resolution (stdin / synthetic sources).
func assetEvalContext(manifestPath string) *hcl.EvalContext {
	return &hcl.EvalContext{
		Functions: map[string]function.Function{
			"file": fileFunc(manifestPath),
			"url":  urlFunc(),
		},
	}
}

// fileFunc reads a local file at HCL parse time and emits a
// tagged cty object the rest of the pipeline carries verbatim.
// Bytes are base64-encoded so the JSON wire shape stays
// portable for binary configs (TLS certs, key files, etc.) —
// the server decodes before writing to disk.
//
// Path resolution: relative paths are anchored at the directory
// of the manifest being parsed (matches Terraform's
// `${path.module}` semantics), so operators run `vd apply -f`
// from anywhere and the file references still resolve. Absolute
// paths are honoured as-is. When the parser has no manifest
// path (stdin), resolution falls back to CWD.
func fileFunc(manifestPath string) function.Function {
	return function.New(&function.Spec{
		Params: []function.Parameter{
			{Name: "path", Type: cty.String},
		},
		Type: function.StaticReturnType(cty.Object(map[string]cty.Type{
			"_source":  cty.String,
			"content":  cty.String,
			"filename": cty.String,
		})),
		Impl: func(args []cty.Value, _ cty.Type) (cty.Value, error) {
			path := args[0].AsString()

			resolved := resolveAssetPath(path, manifestPath)

			bytes, err := os.ReadFile(resolved)
			if err != nil {
				return cty.NilVal, fmt.Errorf("file(%q): %w", path, err)
			}

			return cty.ObjectVal(map[string]cty.Value{
				"_source":  cty.StringVal("file"),
				"content":  cty.StringVal(base64.StdEncoding.EncodeToString(bytes)),
				"filename": cty.StringVal(filepath.Base(resolved)),
			}), nil
		},
	})
}

// resolveAssetPath anchors a `file()` argument against the
// manifest's directory unless the path is absolute. Empty
// manifestPath (stdin / synthetic) falls back to CWD-relative
// resolution — preserves the previous behaviour for callers
// that don't have a manifest on disk.
func resolveAssetPath(path, manifestPath string) string {
	if filepath.IsAbs(path) || manifestPath == "" {
		return path
	}

	return filepath.Join(filepath.Dir(manifestPath), path)
}

// urlFunc emits a tagged cty object the server fetches at
// reconcile time. The URL itself isn't validated here — leaving
// validation server-side keeps the parser fast and lets a
// future `vd lint` step catch URL typos before apply.
//
// Pre-signed URLs (S3/R2) are the recommended way to source
// private assets: include the signature in the URL itself; no
// auth header plumbing required client- or server-side.
// `Authorization: Bearer …` headers can land later as a
// follow-up if pre-signed URLs prove inadequate.
func urlFunc() function.Function {
	return function.New(&function.Spec{
		Params: []function.Parameter{
			{Name: "url", Type: cty.String},
		},
		Type: function.StaticReturnType(cty.Object(map[string]cty.Type{
			"_source": cty.String,
			"url":     cty.String,
		})),
		Impl: func(args []cty.Value, _ cty.Type) (cty.Value, error) {
			return cty.ObjectVal(map[string]cty.Value{
				"_source": cty.StringVal("url"),
				"url":     args[0],
			}), nil
		},
	})
}
