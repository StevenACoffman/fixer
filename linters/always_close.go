package linters

// This file contains AlwaysCloseAnalyzer which helps make sure objects that
// conform to the io.Closable interface is closed. Knowing when and if a
// closeable is closed is actually a pretty hard problem. These variables are
// passed into other functions, stored in other objects, returned back to the
// caller. In many of these cases we don't know what's happening and
// understanding it would require a much deeper inspection to figure out.
// This implementation is a first crack where we ignore many of these edge
// cases assuming it's closed to help prevent false positives of the
// detection.
// There are two good ways to close a  closer object - calling Close(), or
// passing to a function annotated with //ka:closer. The others are really just
// limitations

import (
	"go/ast"
	"go/types"
	"strings"

	"golang.org/x/tools/go/analysis"

	"github.com/StevenACoffman/fixer/lintutil"
)

// Fact exported for a *types.Func when the function is a permission checker.
//
// See the docs for more about Facts:
// https://pkg.go.dev/golang.org/x/tools/go/analysis?tab=doc#hdr-Modular_analysis_with_Facts
type _isCloser struct{}

// AFact tells go/analysis that this is a valid fact type.
func (*_isCloser) AFact() {}

// name is used in tests where we can say // want name "_isCloser"
func (*_isCloser) String() string { return "_isCloser" }

var AlwaysCloseAnalyzer = &analysis.Analyzer{
	Name:      "always_close",
	Doc:       "makes sure close is always called on closeable objects",
	Run:       _runAlwaysClose,
	FactTypes: []analysis.Fact{new(_isCloser)},
}

func _isCloseMethod(method types.Object) bool {
	return strings.HasSuffix(method.String(), ".Close() error")
}

func _isClosable(obj types.Object) bool {
	if obj == nil {
		return false
	}

	method, _, _ := types.LookupFieldOrMethod(obj.Type(), true, nil, "Close")

	return method != nil && _isCloseMethod(method)
}

// We don't need to close closer objects that come from io.NopCloser
func _assignedFromNoOp(rhs []ast.Expr, i int) bool {
	var from ast.Expr
	if len(rhs) == 1 {
		from = rhs[0]
	} else {
		from = rhs[i]
	}

	// Casts don't create new objects, we don't need to track
	// them separately.
	if _, isCast := from.(*ast.TypeAssertExpr); isCast {
		return true
	}
	callExpr, ok := from.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := callExpr.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}

	return sel.Sel.Name == "NopCloser"
}

func _identFromExpression(e ast.Expr) *ast.Ident {
	switch i := e.(type) {
	case *ast.Ident: // abc
		return i
	case *ast.SelectorExpr: // if abc.def, we want def
		return _identFromExpression(i.Sel)
	case *ast.UnaryExpr: // if &abc, we want abc
		return _identFromExpression(i.X)
	case *ast.StarExpr: // if *abc, we want abc
		return _identFromExpression(i.X)
	case *ast.ParenExpr: // if (abc), we want abc
		return _identFromExpression(i.X)
	case *ast.CallExpr: // break down the function
		return _identFromExpression(i.Fun)
	case *ast.KeyValueExpr:
		return _identFromExpression(i.Value)
	}

	return nil
}

func _isCallToCloser(pass *analysis.Pass, callExpr *ast.CallExpr) bool {
	ident := _identFromExpression(callExpr.Fun)
	if ident == nil {
		return false
	}
	obj := lintutil.ObjectFor(ident, pass.TypesInfo)

	return pass.ImportObjectFact(obj, new(_isCloser))
}

func _scanFuncDecl(pass *analysis.Pass, funcDecl *ast.FuncDecl) {
	closables := make(map[types.Object]ast.Node)
	ast.Inspect(funcDecl, func(n ast.Node) bool {
		if node, ok := n.(*ast.AssignStmt); ok {
			for i, variable := range node.Lhs {
				if _, ok := variable.(*ast.Ident); ok {
					obj := lintutil.ObjectFor(variable, pass.TypesInfo)
					if _isClosable(obj) && !_assignedFromNoOp(node.Rhs, i) {
						closables[obj] = variable
					}
				}
			}
		}

		return true
	})

	if len(closables) == 0 {
		return
	}
	ast.Inspect(funcDecl, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.CallExpr:
			if _isCallToCloser(pass, node) {
				// If the function is annotated as a closer function, we mark
				// all of the arguments as 'closed'
				for _, arg := range node.Args {
					delete(closables, lintutil.ObjectFor(arg, pass.TypesInfo))
				}
			} else if sel, ok := node.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Close" {
				delete(closables, lintutil.ObjectFor(sel.X, pass.TypesInfo))
			}
		case *ast.AssignStmt:
			// If we are assigning the closable to another object
			// we don't track these anymore because figuring out if and where
			// it should be closed becomes too complicated
			for _, expr := range node.Rhs {
				ident := _identFromExpression(expr)
				obj := lintutil.ObjectFor(ident, pass.TypesInfo)
				delete(closables, obj)
			}
		case *ast.CompositeLit:
			// Storing a closer object in another object means we cannot
			// track this any longer.
			for _, expr := range node.Elts {
				ident := _identFromExpression(expr)
				obj := lintutil.ObjectFor(ident, pass.TypesInfo)
				delete(closables, obj)
			}
		case *ast.ReturnStmt:
			// Returning a closer means we don't really know what the scope
			// of it is.
			for _, expr := range node.Results {
				ident := _identFromExpression(expr)
				if ident != nil {
					obj := lintutil.ObjectFor(ident, pass.TypesInfo)
					delete(closables, obj)
				}
			}
		}

		return true
	})

	for obj, unclosed := range closables {
		pass.Reportf(unclosed.Pos(), obj.Type().String()+" needs to be closed")
	}
}

func _maybeMarkFunctionAsCloser(pass *analysis.Pass, funcDecl *ast.FuncDecl) {
	if funcDecl.Doc == nil {
		return
	}
	for _, doc := range funcDecl.Doc.List {
		if strings.HasPrefix(doc.Text, "//ka:closer") {
			obj := lintutil.ObjectFor(funcDecl.Name, pass.TypesInfo)
			pass.ExportObjectFact(obj, new(_isCloser))
		}
	}
}

func _scanForClosingFunctions(pass *analysis.Pass) {
	for _, file := range pass.Files {
		for _, decl := range file.Decls {
			funcDecl, ok := decl.(*ast.FuncDecl)
			if ok {
				_maybeMarkFunctionAsCloser(pass, funcDecl)
			}
		}
	}
}

func _runAlwaysClose(pass *analysis.Pass) (interface{}, error) {
	_scanForClosingFunctions(pass)
	for _, file := range pass.Files {
		for _, decl := range file.Decls {
			funcDecl, ok := decl.(*ast.FuncDecl)
			if ok {
				_scanFuncDecl(pass, funcDecl)
			}
		}
	}

	return nil, nil
}
