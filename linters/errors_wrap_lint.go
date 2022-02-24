package linters

// This file contains static analysis of errors.KhanWrap().  It makes sure
// that the Wrap() call, which is sadly untyped, follows the type rules
// we need:
//    errors.KhanWrap(error obj, string, anything, string, anything, ...)

import (
	"go/ast"
	"go/token"

	"golang.org/x/tools/go/analysis"

	"github.com/StevenACoffman/fixer/lintutil"
)

var ErrorsWrapAnalyzer = &analysis.Analyzer{
	Name: "errorswrap",
	Doc:  "verifies arguments to errors.KhanWrap()",
	Run:  _runErrorsWrap,
}

func _runErrorsWrap(pass *analysis.Pass) (interface{}, error) {
	for _, file := range pass.Files {
		ast.Inspect(file, func(node ast.Node) bool {
			// Look for calls to errors.Wrap
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true // recurse
			}

			fnObj := lintutil.ObjectFor(call.Fun, pass.TypesInfo)
			if lintutil.NameOf(fnObj) != "github.com/Khan/webapp/pkg/lib/errors.Wrap" {
				return true
			}

			if len(call.Args) == 2 && call.Ellipsis != token.NoPos {
				// We can't really lint if you use a slice of varargs.
				// TODO(benkraft): Should we just disallow that?
				return true
			}

			// Rule #1: there should be an odd number of arguments.
			if len(call.Args)%2 == 0 {
				pass.Reportf(call.Pos(),
					"errors.KhanWrap() should have an odd number of arguments, not %v",
					len(call.Args))

				return true
			}

			// Rule #2: each odd arg must be a string literal.
			for i := 1; i < len(call.Args); i += 2 {
				argLit, ok := call.Args[i].(*ast.BasicLit)
				if !ok || argLit.Kind != token.STRING {
					argType := pass.TypesInfo.TypeOf(call.Args[i])
					pass.Reportf(
						call.Args[i].Pos(),
						"errors.KhanWrap() should use string-literals as keys, but arg %v has type %v",
						// TODO(csilvers): extract and use _shortTypeName from
						// pkg/kacontext/linters/interface_lint.go.
						i,
						argType.String(),
					)
				}
			}

			return true
		})
	}

	return nil, nil
}
