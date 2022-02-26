package linters

// This file contains lint checks that we *either* return *or* log an error,
// but never both.

import (
	"go/ast"
	"go/token"
	"go/types"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/ctrlflow"
	"golang.org/x/tools/go/cfg"

	"github.com/StevenACoffman/fixer/lintutil"
)

// LogOrReturnErrorAnalyzer checks that go errors are either logged or
// returned, but not both.
//
// This prevents errors from being logged multiple times at different levels
// in the stack.
var LogOrReturnErrorAnalyzer = &analysis.Analyzer{
	Name:     "log_or_return_error",
	Doc:      "requires that errors either be logged or returned, but not both",
	Run:      _runLogOrReturnError,
	Requires: []*analysis.Analyzer{ctrlflow.Analyzer},
}

// _errorResultIndices returns the indices of the error-results for this
// function-signature.
func _errorResultIndices(sig *ast.FuncType) []int {
	indices := []int{}
	if sig.Results == nil {
		return indices
	}

	i := 0
	for _, result := range sig.Results.List {
		ident, ok := result.Type.(*ast.Ident)
		if ok && ident.Name == "error" {
			indices = append(indices, i)

			continue
		}

		if len(result.Names) > 0 {
			// Named result(s): we might have `a, b T` in which case we need to
			// count this as 2 results.
			i += len(result.Names)
		} else {
			// Unnamed result: it's exactly one result.
			i++
		}
	}

	return indices
}

func _runLogOrReturnError(pass *analysis.Pass) (interface{}, error) {
	cfgs, _ := pass.ResultOf[ctrlflow.Analyzer].(*ctrlflow.CFGs)
	for _, file := range pass.Files {
		ast.Inspect(file, func(node ast.Node) bool {
			switch node := node.(type) {
			case *ast.FuncDecl:
				_lintLogOrReturnErrors(pass, _errorResultIndices(node.Type), cfgs.FuncDecl(node))
			case *ast.FuncLit:
				_lintLogOrReturnErrors(pass, _errorResultIndices(node.Type), cfgs.FuncLit(node))
			}

			return true
		})
	}

	return nil, nil
}

// :pass: is used to look up the types of identifiers
// :errorReturnIndices: indicates while return values are of type `error`,
// so we can ignore places where errors get referenced in non-error return
// values, such as when mapping an error into a graphql response.
// :block: is the current control flow block to inspect.
// :loggedErrors: contain a mapping of identifiers that are errors
// from previous blocks, mapped to the `token.Pos` file position where the
// call to ctx.Log().XYZ() occurred.
// - if the value is nil, then the id has not been logged yet
// - if the value is non-nil, then it has been logged at the given pos
// :traversedPaths: is a mapping of (blockid, blockid) to "number of times this
// edge of the control flow graph has been traversed". We allow traversing an
// edge twice, so we can account for for loops, but not more than that.
func _checkBlockForLogsAndReturns(
	pass *analysis.Pass,
	errorReturnIndices []int,
	block *cfg.Block,
	loggedErrors map[types.Object]*token.Pos,
	traversedPaths map[[2]int32]int,
) {
	if !block.Live {
		return
	}

	for _, blockNode := range block.Nodes {
		ast.Inspect(blockNode, func(node ast.Node) bool {
			switch node := node.(type) {
			// look for assignments that are of type 'error', such as
			//     err = errors.Internal()
			// or
			//     value, err := someCall()
			case *ast.AssignStmt:
				for _, target := range node.Lhs {
					if ident, ok := target.(*ast.Ident); ok {
						errorObj := lintutil.ObjectFor(ident, pass.TypesInfo)
						if errorObj != nil && errorObj.Type().String() == "error" {
							// not yet logged, just now assigned
							loggedErrors[errorObj] = nil
						}
					}
				}
			// Subsequently, if an identifier known to be an error is passed to
			// ctx.Log().Something(), mark it as having been logged.
			case *ast.CallExpr:
				// We only care about call expressions to `ctx.Log().XYZ()`
				if !receiverIsLogger(lintutil.ObjectFor(node.Fun, pass.TypesInfo)) {
					return true
				}
				// Within the call expression to ctx.Log().XYZ(), look for
				// identifiers that we've previously determined are errors
				ast.Inspect(node, func(node ast.Node) bool {
					ident, ok := node.(*ast.Ident)
					if !ok {
						return true
					}
					errorObj := lintutil.ObjectFor(ident, pass.TypesInfo)
					if errorObj == nil {
						return true
					}
					alreadyLogged, ok := loggedErrors[errorObj]
					if !ok {
						return true
					}
					if alreadyLogged != nil {
						pass.Reportf(ident.Pos(),
							"This error is being logged twice. Previously logged at %v. See "+
								"https://khanacademy.atlassian.net/l/c/j9btTeyW",
							pass.Fset.File(*alreadyLogged).Position(*alreadyLogged))
					} else {
						pos := ident.Pos()
						loggedErrors[errorObj] = &pos
					}

					return true
				})
				// No need to recurse further.
				return false
			// Then when inspecting return statements, we can find any errors
			// that have been logged and flag them.
			// Critically, we allow errors to be contained in return values as
			// long as the type of the return value isn't `error`. This is so
			// that we can use the error value when constructing a graphql
			// response, given this common ADR-303 pattern:
			//
			// if err != nil {
			//     ctx.Log().Warn(err)
			//     return _mapErrorToGraphql(err), nil
			// }
			case *ast.ReturnStmt:
				for _, idx := range errorReturnIndices {
					if idx >= len(node.Results) {
						continue
					}
					value := node.Results[idx]
					ast.Inspect(value, func(node ast.Node) bool {
						if ident, ok := node.(*ast.Ident); ok {
							errorObj := lintutil.ObjectFor(ident, pass.TypesInfo)
							if errorObj == nil {
								return true
							}
							if logPos := loggedErrors[errorObj]; logPos != nil {
								pass.Reportf(ident.Pos(),
									"Errors may be logged or returned, but not both. Previously "+
										"logged at %v. See "+
										"https://khanacademy.atlassian.net/l/c/j9btTeyW",
									pass.Fset.File(*logPos).Position(*logPos))
							}
						}

						return true
					})
				}

				return false
			// Don't recurse into function expression literals; those will
			// be covered by `_runLogOrReturnError`.
			case *ast.FuncLit:
				return false
			}

			return true // otherwise, recurse
		})
	}

	for i, succ := range block.Succs {
		if times, ok := traversedPaths[[2]int32{block.Index, succ.Index}]; ok {
			// don't need to retrace this edge
			if times > 1 {
				continue
			}
			traversedPaths[[2]int32{block.Index, succ.Index}] = times + 1
		} else {
			traversedPaths[[2]int32{block.Index, succ.Index}] = 1
		}

		forChild := loggedErrors
		if i < len(block.Succs)-1 {
			// got to copy the map, except for passing to the last succ
			forChild = make(map[types.Object]*token.Pos)
			for k, v := range loggedErrors {
				forChild[k] = v
			}
		}
		_checkBlockForLogsAndReturns(pass, errorReturnIndices, succ, forChild, traversedPaths)
	}
}

// _lintLogOrReturnErrors reports a lint error any time an error is both logged
// and returned, or is logged twice in a row.
func _lintLogOrReturnErrors(pass *analysis.Pass, errorReturnIndices []int, graph *cfg.CFG) {
	_checkBlockForLogsAndReturns(
		pass, errorReturnIndices, graph.Blocks[0],
		make(map[types.Object]*token.Pos), make(map[[2]int32]int))
}
