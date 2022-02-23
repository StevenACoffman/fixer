package linters

// This file contains linting logic for checking arguments in errors.Is and
// errors.As which are often confusing.
//
// errors.Is expects two errors: a local one, and a sentinel and in that
// order. This is so that you can check if the err you received from a
// function is actually the same error you're expecting despite any
// wrapping that might have been done. errors.As does something similar: it
// checks the type of your local err against a reference to an example of
// another type of err (like datastore.MultiError). This is important
// because the complier doesn't care if you get the order wrong or don't
// take the reference to an error type for error.As which are common
// mistakes that still actually work in some situations.
//
// To do this, the linter does a bunch of heavy lifting to detect something
// which should be simple: which variables are local and which are not. To
// figure out what variables are local, you need to look at all the
// different places local variables come from (if statements, for loops,
// ranges, function arguments, and declarations). We scan through a
// function getting all the local variables and make sure the first
// argument in errors.Is and error.As is one of them. Once that is done the
// 2nd check is pretty simple: make sure the 2nd argument to errors.Is is
// not a local variable and that errors.As is taking the reference to a
// variable. If all of these checks pass, this linter is a happy camper.
import (
	"go/ast"
	"go/types"

	"golang.org/x/tools/go/analysis"

	"github.com/StevenACoffman/fixer/lintutil"
)

// ErrorArgumentAnalyzer runs lint checks that we have the desired
// documentation.
var ErrorArgumentAnalyzer = &analysis.Analyzer{
	Name: "errorsarguments",
	Doc:  "ensures all files and packages are documented",
	Run:  _runErrorsArgument,
}

func _importsKaErrors(file *ast.File) bool {
	for _, imp := range file.Imports {
		if imp.Path.Value == "\"github.com/Khan/webapp/pkg/lib/errors\"" ||
			imp.Path.Value == "\"errors\"" {
			return true
		}
	}
	return false
}

func _isLocalError(err types.Object, localVariables []types.Object) bool {
	for _, localError := range localVariables {
		if localError == err {
			return true
		}
	}

	return false
}

func _scrapeParamaterListForLocalVariables(
	pass *analysis.Pass,
	fields []*ast.Field,
	localVariables []types.Object,
) []types.Object {
	for _, f := range fields {
		for _, name := range f.Names {
			argType := pass.TypesInfo.TypeOf(name)
			if argType != nil {
				localVariables = append(localVariables, lintutil.ObjectFor(name, pass.TypesInfo))
			}
		}
	}
	return localVariables
}

func _scrapeAssignmentForLocalVariables(
	pass *analysis.Pass,
	assign *ast.AssignStmt,
	localVariables []types.Object,
) []types.Object {
	for _, v := range assign.Lhs {
		if v == nil { // if the variable is _, then v is nil
			continue
		}
		argType := pass.TypesInfo.TypeOf(v)
		if argType != nil {
			localVariables = append(localVariables, lintutil.ObjectFor(v, pass.TypesInfo))
		}
	}
	return localVariables
}

// This function is called recursively to unwrap various parts of the
// expression to get at the ident that is being referenced. This covers all
// cases in the current code base, but there could be more.
func _expressionIsLocalVariable(
	pass *analysis.Pass,
	expr ast.Expr,
	localVariables []types.Object,
) bool {
	switch n := expr.(type) {
	case *ast.Ident:
		// n.IsExported mostly catches package public idents like the
		// datastore.NotFoundErr without needing to go through the loop.
		return !n.IsExported() && _isLocalError(
			lintutil.ObjectFor(expr, pass.TypesInfo), localVariables)
	case *ast.CallExpr: // we have v.someFunc(), v.Fun gives us a SelectorExpr
		// which recursively will give us just v.
		return _expressionIsLocalVariable(pass, n.Fun, localVariables)
	case *ast.SelectorExpr: // we have v.someError, we need to know if v is local.
		return _expressionIsLocalVariable(pass, n.X, localVariables)
	case *ast.UnaryExpr: // when we have &v, we want just v
		return _expressionIsLocalVariable(pass, n.X, localVariables)
	case *ast.IndexExpr: // when we have v[i], we want v and not i
		return _expressionIsLocalVariable(pass, n.X, localVariables)
	}
	return false
}

func _inspectFunction(pass *analysis.Pass, node ast.Node) {
	localVariables := make([]types.Object, 0)
	// Step 1: Scrape all the local errors out of the function
	ast.Inspect(node, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.AssignStmt: // extracts a from "a := 0"
			localVariables = _scrapeAssignmentForLocalVariables(pass, n, localVariables)
		case *ast.GenDecl: // extracts a,b,c from "var a, b, c error"
			for _, spec := range n.Specs {
				if valueSpec, ok := spec.(*ast.ValueSpec); ok {
					for _, name := range valueSpec.Names {
						localVariables = append(
							localVariables, lintutil.ObjectFor(name, pass.TypesInfo))
					}
				}
			}
		case *ast.FuncType: // extracts a, b from "func [name](a, b, c)"
			// this coveres the top level funcDecl and any
			// funcLits contained within.
			if n.Params != nil {
				localVariables = _scrapeParamaterListForLocalVariables(
					pass, n.Params.List, localVariables)
			}
		case *ast.RangeStmt: // extracts k, v from "for k,v := range someList"
			if n.Value != nil {
				localVariables = append(
					localVariables, lintutil.ObjectFor(n.Value, pass.TypesInfo))
			}
			if n.Key != nil {
				localVariables = append(
					localVariables, lintutil.ObjectFor(n.Key, pass.TypesInfo))
			}
		}
		return true
	})

	// Step 2, find all the calls to errors.Is and errors.As see if any of them
	// use a non-local error as the first argument.
	ast.Inspect(node, func(node ast.Node) bool {
		callExpr, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		name := lintutil.NameOf(lintutil.ObjectFor(callExpr.Fun, pass.TypesInfo))
		if len(callExpr.Args) != 2 {
			return true
		}
		err := callExpr.Args[0]
		target := callExpr.Args[1]
		if name == "github.com/Khan/webapp/pkg/lib/errors.Is" ||
			name == "errors.Is" {
			if !_expressionIsLocalVariable(pass, err, localVariables) {
				pass.Reportf(err.Pos(), "First argument to errors.Is needs to be a local variable")
			}

			if _expressionIsLocalVariable(pass, target, localVariables) {
				pass.Reportf(target.Pos(), "Second argument to errors.Is cannot be a local variable")
			}
		} else if name == "github.com/Khan/webapp/pkg/lib/errors.As" || name == "errors.As" {
			if !_expressionIsLocalVariable(pass, err, localVariables) {
				pass.Reportf(err.Pos(), "First argument to errors.As needs to be a local variable")
			}
			// error.As is a bit different. The second argument is only allowed
			// to be a reference to a variable, we don't care about scope.
			// TODO (jeremygervais): We're just checking if we are taking
			// the ref of something here. Not that the something is a
			// pointer already or if it's even an error. We can do better!
			if _, ok := target.(*ast.UnaryExpr); !ok {
				pass.Reportf(callExpr.Args[1].Pos(), "The second argument of errors.As must take the reference to an object")
			}
		}
		return true
	})
}

func _runErrorsArgument(pass *analysis.Pass) (interface{}, error) {
	for _, file := range pass.Files {
		// Quick optimization, check to see if the file imports
		// "github.com/StevenACoffman/fixer/errors", if not, we can skip it.
		if !_importsKaErrors(file) {
			continue
		}

		// We only care about the functions at this level, but they're
		// declared two different ways. The first is easy: funcDecl we just
		// inspect it. FuncLits are the other way which require us to parse
		// the declaration of the valueSpec to get at the actual funcLit
		// which we can then inspect.
		for _, decl := range file.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				_inspectFunction(pass, decl)
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					if valueSpec, isValue := spec.(*ast.ValueSpec); isValue {
						for _, val := range valueSpec.Values {
							if _, isFuncLit := val.(*ast.FuncLit); isFuncLit {
								_inspectFunction(pass, val)
							}
						}
					}
				}
			}
		}
	}
	return nil, nil
}
