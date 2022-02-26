// Package linters contains logging-related linters, in particular checking we
// don't log an Sprintf'ed value (but use fields instead).
package linters

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"strings"

	"golang.org/x/tools/go/analysis"

	"github.com/StevenACoffman/fixer/lintutil"
)

var LogAnalyzer = &analysis.Analyzer{
	Name: "log",
	Doc:  "checks that we don't log a Sprintf'ed value (we use fields instead)",
	Run:  _runLog,
}

// _isSprintf returns true if the expr is a Sprintf-like function.
func _isSprintf(expr ast.Expr, typesInfo *types.Info) bool {
	name := lintutil.NameOf(lintutil.ObjectFor(expr, typesInfo))

	return strings.HasPrefix(name, "fmt.Sprint") // Sprintf, Sprintln, etc.
}

// Get all the identifiers whose values may be fmt.Sprintf calls.
//
// For example, from
//  msg := fmt.Sprintf(...)
// we would have an entry [Object msg] -> [CallExpr fmt.Sprintf(...)].
//
// TODO(benkraft): I suspect this might be simpler and more precise using the
// SSA representation built by x/tools/go/analysis/passes/buildssa, but making
// sense of it seems more work than it's worth to understand right now.
func _sprintfs(file *ast.File, typesInfo *types.Info) map[types.Object]*ast.CallExpr {
	retval := map[types.Object]*ast.CallExpr{}
	// Given an assignment `lhs = rhs`, add the corresponding entry to retval
	// if rhs is a fmt.Sprintf call.
	maybeAddSprintf := func(lhs, rhs ast.Node) {
		lhsIdent, lhsOk := lhs.(*ast.Ident)
		rhsCall, rhsOk := rhs.(*ast.CallExpr)
		if rhsOk && lhsOk && _isSprintf(rhsCall.Fun, typesInfo) {
			// In := this will be a Def; in = it will be a Use; ObjectOf
			// checks both.
			retval[typesInfo.ObjectOf(lhsIdent)] = rhsCall
		}
	}

	ast.Inspect(file, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.AssignStmt:
			// sprintf never returns multiple values, but it could be used in
			// e.g. `a, b := fmt.Sprintf(...), "other string"`, in which case
			// we want to zip the two together.
			if len(node.Lhs) == len(node.Rhs) {
				for i, lhs := range node.Lhs {
					maybeAddSprintf(lhs, node.Rhs[i])
				}
			}
		case *ast.GenDecl:
			if node.Tok == token.CONST || node.Tok == token.VAR {
				for _, spec := range node.Specs {
					// the cast is guaranteed based on node.Tok
					valueSpec, _ := spec.(*ast.ValueSpec)
					// As above, we might be unpacking multiple returns,
					// assigning multiple unrelated names at once, or in this
					// case we might have no values at all (`var name type`).
					if len(valueSpec.Names) == len(valueSpec.Values) {
						for i, lhs := range valueSpec.Names {
							maybeAddSprintf(lhs, valueSpec.Values[i])
						}
					}
				}
			}
		}

		return true // always recurse
	})

	return retval
}

func receiverIsLogger(funcObj types.Object) bool {
	if funcObj == nil {
		return false
	}
	funcType, ok := funcObj.Type().(*types.Signature)
	if !ok {
		return false
	}
	recv := funcType.Recv()
	if recv == nil {
		return false
	}
	recvType := recv.Type()
	// TODO(benkraft): If we ever have methods of Logger for which printfs
	// *are* valid arguments, check funcObj.Name() as well.
	return recvType != nil &&
		recvType.String() == "github.com/Khan/webapp/pkg/lib/log.Logger"
}

// _isBadLoggingCall checks if the node is a call to log using Sprintf, and if
// so, returns the Sprintf expression.
//
// sprintfs should be the return value of _sprintfs().
func _isBadLoggingCall(
	node ast.Node,
	sprintfs map[types.Object]*ast.CallExpr,
	typesInfo *types.Info,
) *ast.CallExpr {
	call, ok := node.(*ast.CallExpr)
	if !ok {
		return nil
	}

	funcObj := lintutil.ObjectFor(call.Fun, typesInfo)
	if !receiverIsLogger(funcObj) {
		return nil
	}

	// We're interested in the first arg, the message
	if len(call.Args) < 1 { // the arg is required, but just to be safe
		return nil
	}
	switch messageArg := call.Args[0].(type) {
	case *ast.CallExpr:
		// If the node is a Sprintf call, complain.
		if _isSprintf(messageArg.Fun, typesInfo) {
			return messageArg
		}
	case *ast.Ident:
		// If the node references a Sprintf call, also complain.
		return sprintfs[typesInfo.Uses[messageArg]]
	}

	return nil
}

func _runLog(pass *analysis.Pass) (interface{}, error) {
	for _, file := range pass.Files {
		// It's common to do things like
		//  msg := fmt.Sprintf(...)
		//  ctx.Log().Info(msg)
		// so we make sure to check for that, at least in its most obvious
		// form.  (Of course a perfect check is impossible.)
		sprintfs := _sprintfs(file, pass.TypesInfo)
		ast.Inspect(file, func(node ast.Node) bool {
			badSprintf := _isBadLoggingCall(node, sprintfs, pass.TypesInfo)
			if badSprintf != nil {
				var loc string
				if node.Pos() > badSprintf.Pos() || badSprintf.Pos() > node.End() {
					// If the Sprintf is not inline in the call, point to where
					// it is.
					loc = fmt.Sprintf(" (at %s)",
						pass.Fset.Position(badSprintf.Pos()).String())
				}
				pass.Reportf(node.Pos(),
					"avoid using Sprintf%s to log; "+
						"instead use a fixed message with fields",
					loc)
			}

			return true // always recurse
		})
	}

	return nil, nil
}
