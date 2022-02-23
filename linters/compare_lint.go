package linters

// This file contains CompareAnalyzer. The analyzer checks for binary
// comparison operators like ==, !=, <=, >=, < and > and sees if the
// objects involved implement a Equal, Before or After method. If it does
// it reccomends that those are used instead.
// TODO: There are likely a few other methods that could be checked rather than
// just Before and After. See about adding more methods to search for.

import (
	"go/ast"
	"go/token"
	"go/types"

	"golang.org/x/tools/go/analysis"

	"github.com/StevenACoffman/fixer/lintutil"
)

var CompareAnalyzer = &analysis.Analyzer{
	Name: "compare",
	Doc:  "Ensures that objects with Equals and Compare functions are compared correctly", // TODO
	Run:  _runCompareLint,
}

func _hasEqualMethod(obj types.Object) bool {
	method, _, _ := types.LookupFieldOrMethod(obj.Type(), true, nil, "Equal")
	if method == nil {
		return false
	}

	sig, ok := method.Type().(*types.Signature)
	return ok &&
		sig.Results().Len() == 1 &&
		sig.Results().At(0).String() == "var  bool"
}

func _hasBeforeMethod(obj types.Object) bool {
	method, _, _ := types.LookupFieldOrMethod(obj.Type(), true, nil, "Before")
	if method == nil {
		return false
	}
	sig, ok := method.Type().(*types.Signature)
	return ok &&
		sig.Results().Len() == 1 &&
		sig.Results().At(0).String() == "var  bool"
}

func _hasAfterMethod(obj types.Object) bool {
	method, _, _ := types.LookupFieldOrMethod(obj.Type(), true, nil, "After")
	if method == nil {
		return false
	}
	sig, ok := method.Type().(*types.Signature)
	return ok &&
		sig.Results().Len() == 1 &&
		sig.Results().At(0).String() == "var  bool"
}

func _checkIsPointer(pass *analysis.Pass, left types.Object, right types.Object, pos token.Pos) {
	if _, isLeftPointer := left.Type().(*types.Pointer); isLeftPointer {
		pass.Reportf(pos, "Left hand side is a pointer. You probably don't intend to be comparing that")
	}
	if _, isRightPointer := right.Type().(*types.Pointer); isRightPointer {
		pass.Reportf(pos, "Right hand side is a pointer. You probably don't intend to be comparing that")
	}
}

func _checkEquals(pass *analysis.Pass, left types.Object, right types.Object, pos token.Pos) {
	if _hasEqualMethod(left) {
		pass.Reportf(pos, "Left hand side implements Equals method. Use that instead.")
	} else if _hasEqualMethod(right) {
		pass.Reportf(pos, "Right hand side implements Equals method. Use that instead.")
	}
}

func _checkBeforeAfter(pass *analysis.Pass, left types.Object, right types.Object, pos token.Pos) {
	if _hasAfterMethod(left) || _hasBeforeMethod(left) {
		pass.Reportf(pos, "Left hand side implements Before/After methods. Use that instead.")
	} else if _hasAfterMethod(right) || _hasBeforeMethod(right) {
		pass.Reportf(pos, "Right hand side implements Before/After method. Use that instead.")
	}
}

func _checkBinaryExpression(pass *analysis.Pass, expr *ast.BinaryExpr) {
	x := lintutil.ObjectFor(expr.X, pass.TypesInfo)
	y := lintutil.ObjectFor(expr.Y, pass.TypesInfo)
	if x == nil || y == nil { // these are generally numbers
		return
	}
	xName := lintutil.NameOf(x)
	yName := lintutil.NameOf(y)
	if xName == "" || yName == "" { // these are nils
		return
	}
	switch expr.Op {
	case token.EQL, token.NEQ:
		_checkEquals(pass, x, y, expr.Pos())
		_checkIsPointer(pass, x, y, expr.Pos())
	case token.LEQ, token.GEQ:
		_checkEquals(pass, x, y, expr.Pos())
		_checkBeforeAfter(pass, x, y, expr.Pos())
		_checkIsPointer(pass, x, y, expr.Pos())
	case token.LSS, token.GTR:
		_checkBeforeAfter(pass, x, y, expr.Pos())
		_checkIsPointer(pass, x, y, expr.Pos())
	}
}

func _runCompareLint(pass *analysis.Pass) (interface{}, error) {
	for _, file := range pass.Files {
		ast.Inspect(file, func(n ast.Node) bool {
			if op, ok := n.(*ast.BinaryExpr); ok {
				_checkBinaryExpression(pass, op)
			}
			return true
		})
	}
	return nil, nil
}
