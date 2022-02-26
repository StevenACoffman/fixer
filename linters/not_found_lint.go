package linters

// This file contains a linter to ensure we return errors.NotFound(...) instead
// of just a nil model if the model is not found.
//
// Actually, it just checks that we don't return a nil model except when we are
// returning a non-nil error.

import (
	"go/ast"
	"go/token"
	"go/types"
	"strings"

	"golang.org/x/tools/go/analysis"

	"github.com/StevenACoffman/fixer/lintutil"
)

var NotFoundAnalyzer = &analysis.Analyzer{
	Name: "not_found",
	Doc:  "enforces that we don't return (nil, nil) to mean not-found",
	Run:  _runNotFound,
}

func _isNil(obj types.Object) bool {
	_, ok := obj.(*types.Nil)

	return ok
}

func _isError(typ types.Type) bool {
	return types.Identical(typ, types.Universe.Lookup("error").Type())
}

// If the expression is any of var, &var, var[i], &var[i], etc., return the
// types.Object representing var.  Else, return nil.
func _referencedVar(expr ast.Expr, typesInfo *types.Info) types.Object {
	for {
		obj := lintutil.ObjectFor(expr, typesInfo)
		if obj != nil {
			if _isNil(obj) {
				// go/types treats nil as a var, but it's not really useful
				// to us to think of it that way.
				return nil
			}

			return obj
		}

		switch typedExpr := expr.(type) {
		case *ast.UnaryExpr:
			if typedExpr.Op == token.AND {
				expr = typedExpr.X

				continue
			}
		case *ast.IndexExpr:
			expr = typedExpr.X

			continue
		}

		return nil
	}
}

// Map from function-name (as defined by lintutil.NameOf) to index of the
// dst parameter.
var _getFns = map[string]int{
	"(github.com/Khan/webapp/pkg/gcloud/datastore.Client).Get":      2,
	"(github.com/Khan/webapp/pkg/gcloud/datastore.Client).GetAll":   2,
	"(github.com/Khan/webapp/pkg/gcloud/datastore.Client).GetMulti": 2,
	"(*github.com/Khan/webapp/pkg/gcloud/datastore.Iterator).Next":  0,
}

func _analyzeFunction(typ *ast.FuncType, body *ast.BlockStmt, pass *analysis.Pass) {
	// Nothing to analyze for a function with no returns.
	if typ.Results == nil || len(typ.Results.List) == 0 {
		return
	}

	// like ast.Inspect(body, f), but don't recurse on nested functions (they
	// are handled separately by the caller, and always recurse otherwise
	inspect := func(f func(ast.Node)) {
		ast.Inspect(body, func(node ast.Node) bool {
			if _, ok := node.(*ast.FuncLit); ok {
				return false // don't recurse
			}
			f(node)

			return true // recurse
		})
	}

	// First, find calls to the functions we're interested in, and extract any
	// variable referenced by the dst argument.
	var firstDatastoreCall token.Pos
	modelVars := map[types.Object]bool{} // set of vars that are dst arguments
	inspect(func(node ast.Node) {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return
		}

		fun := lintutil.ObjectFor(call.Fun, pass.TypesInfo)
		index, ok := _getFns[lintutil.NameOf(fun)]
		if !ok {
			return
		}

		if index >= len(call.Args) { // should never happen
			return
		}

		dst := _referencedVar(call.Args[index], pass.TypesInfo)
		if dst == nil { // not a var (e.g. nil, or something complicated)
			return
		}

		modelVars[dst] = true
		if firstDatastoreCall == 0 {
			firstDatastoreCall = call.Pos()
		}
	})

	if len(modelVars) == 0 {
		return // no datastore calls to look at
	}

	// Next, look for where we return those vars, and keep track of which
	// result-index it was.  (Typically this will be the first one, and the
	// second will be an error, but many other things are possible.)
	nReturns := len(typ.Results.List)
	modelReturns := make([]bool, nReturns) // true ith ret is ever a model-var
	inspect(func(node ast.Node) {
		ret, ok := node.(*ast.ReturnStmt)
		if !ok {
			return
		}

		if len(ret.Results) != len(modelReturns) { // should never happen
			return
		}

		for i, result := range ret.Results {
			obj := _referencedVar(result, pass.TypesInfo)
			if obj != nil && modelVars[obj] {
				modelReturns[i] = true
			}
		}
	})

	// Finally, find anywhere we return nil in one of those result-indices,
	// and don't also return a non-nil error in the last result.
	returnsErr := _isError(pass.TypesInfo.TypeOf(typ.Results.List[nReturns-1].Type))
	inspect(func(node ast.Node) {
		ret, ok := node.(*ast.ReturnStmt)
		if !ok {
			return
		}

		if len(ret.Results) != len(modelReturns) { // should never happen
			return
		}

		if ret.Pos() < firstDatastoreCall {
			// ok to return nil before datastore call (e.g. if input is nil)
			return
		}

		lastReturn := lintutil.ObjectFor(ret.Results[nReturns-1], pass.TypesInfo)
		if returnsErr && !_isNil(lastReturn) {
			// last return is a non-nil error, so it's okay.
			return
		}

		for i, result := range ret.Results {
			if !modelReturns[i] {
				// no model returned in this position, not of interest.
				continue
			}

			if _, ok := lintutil.ObjectFor(result, pass.TypesInfo).(*types.Nil); !ok {
				// return is non-nil, so it's okay.
				continue
			}

			// else report!
			pass.Reportf(result.Pos(),
				"don't return a nil model without an error; "+
					"instead return an explicit NotFound error "+
					"(see https://khanacademy.atlassian.net/l/c/EDG5wQgw#Datastore)")
		}
	})
}

func _runNotFound(pass *analysis.Pass) (interface{}, error) {
	for _, file := range pass.Files {
		// We don't care about tests.
		filename := pass.Fset.File(file.Pos()).Name()
		if strings.HasSuffix(filename, "_test.go") {
			continue
		}

		// Everything else, we go function-by-function.
		ast.Inspect(file, func(node ast.Node) bool {
			switch node := node.(type) {
			case *ast.FuncLit:
				_analyzeFunction(node.Type, node.Body, pass)
			case *ast.FuncDecl:
				_analyzeFunction(node.Type, node.Body, pass)
			}

			return true // recurse
		})

		// TODO(benkraft): We could also trace across functions: if you call
		// a function that does a datastore-get and returns a model, then you,
		// similarly, shouldn't return that model sometimes and nil without err
		// other times.  But we're most concerned about the lowest-level
		// functions here, and doing so would be more work, so we don't bother.
	}

	return nil, nil
}
