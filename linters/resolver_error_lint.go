package linters

// This file contains lint checks that we return errors the right way in our
// GraphQL resolvers.

import (
	"go/ast"

	"golang.org/x/tools/go/analysis"
)

// ResolverErrorAnalyzer checks that GraphQL resolvers return errors correctly.
//
// In ADR-303 [1], we decided that mutation resolvers should not return
// GraphQL-style errors, but should instead return an object with an error
// field.  This linter enforces that convention.
//
// At this time, there are no rules for query resolvers; those are under
// discussion in ADR-366 [2].
// TODO(benkraft): Once that ADR is decided, consider whether it there are
// rules we should lint for.
//
// [1] https://docs.google.com/document/d/1sDCysF-aH1lJlnZC4aAOczVUeIXkkPNDMiJBhb6PnXA/edit
// [2] https://docs.google.com/document/d/1dRvb_VUVEbCHOKJqTHdl3TekEizpcSrcTCpu9oFUk7s/edit
var ResolverErrorAnalyzer = &analysis.Analyzer{
	Name: "resolver_error",
	Doc:  "requires GraphQL resolvers return errors correctly",
	Run:  _runResolverError,
	// We pull the list of resolvers from PermissionsAnalyzer, rather than
	// recomputing it.
	Requires: []*analysis.Analyzer{PermissionsAnalyzer},
}

// _errorResultIndex returns the index of the first error-result to this
// function-signature, or -1 if there is none.
//
// In principle we should perhaps look for the last error-result, but in
// practice there is only one, so it doesn't really matter.  The nontrivial
// part, which is easiest to do from front to end, is actually counting the
// expected number of results.
func _errorResultIndex(sig *ast.FuncType) int {
	if sig.Results == nil {
		return -1
	}

	i := 0
	for _, result := range sig.Results.List {
		ident, ok := result.Type.(*ast.Ident)
		if ok && ident.Name == "error" {
			return i
		}

		if len(result.Names) > 0 {
			// Named result(s): we might have `a, b T` in which case we need to
			// count this as 2 results.
			i += len(result.Names)
		} else {
			// Unnamed result: it's exactly one results.
			i++
		}
	}
	return -1
}

// _lintResolverErrors reports a lint error any time this resolver returns a
// toplevel error (rather than an error field).
func _lintResolverErrors(pass *analysis.Pass, resolver *ast.FuncDecl) {
	errorIndex := _errorResultIndex(resolver.Type)
	if errorIndex == -1 {
		return // somehow the resolver doesn't return an error, assume it's ok
	}

	ast.Inspect(resolver.Body, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.FuncLit:
			// If the resolver defines an inline function, we don't care about
			// returns within that.
			return false
		case *ast.ReturnStmt:
			// This doesn't handle bare returns, or returns of multiple values
			// at once (e.g. from a single function which returns them).  For
			// now we just assume both are ok; in practice both are rare in
			// resolvers.
			// TODO(benkraft): Handle bare returns, perhaps by instead looking
			// for assignments to the error-return's name.
			// TODO(benkraft): Handle multiple returns, perhaps by also linting
			// the callee function.
			if errorIndex < len(node.Results) {
				errorReturn := node.Results[errorIndex]
				ident, ok := errorReturn.(*ast.Ident)
				if !ok || ident.Name != "nil" {
					pass.Reportf(errorReturn.Pos(),
						"Mutation resolvers should not return errors, "+
							"per ADR-303; use an error-field instead")
				}
			}
			return false // no need to recurse, we can't nest return statements
		}
		return true // otherwise, recurse
	})
}

func _runResolverError(pass *analysis.Pass) (interface{}, error) {
	resolvers, ok := pass.ResultOf[PermissionsAnalyzer].([]resolver)
	if !ok {
		panic("dependency return type error")
	}

	for _, resolver := range resolvers {
		if resolver.typeName != "mutationResolver" {
			// we only look at mutation resolvers, at least for now
			continue
		}

		_lintResolverErrors(pass, resolver.f)
	}

	return nil, nil
}
