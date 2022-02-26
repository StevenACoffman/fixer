package linters

// This file contains the analyzer for the userlockmodel linter. This linter
// enforces rules relating to WrittenWithUserLockModel models.

import (
	"go/ast"
	"go/token"
	"go/types"

	"golang.org/x/tools/go/analysis"
)

var UserLockModelAnalyzer = &analysis.Analyzer{
	Name: "userlockmodel",
	Doc:  "enforces rules relating to WrittenWithUserLockModel models",
	Run:  _runUserLockModel,
}

func _runUserLockModel(pass *analysis.Pass) (interface{}, error) {
	// This is an interface that has a GetKAID method, i.e.
	//
	//   interface {
	//       GetKAID() string
	//   }
	//
	// An interface...
	kaiderInterface := types.NewInterfaceType(
		[]*types.Func{
			// ... with one function
			types.NewFunc(
				token.NoPos,
				// ... without an associated package
				nil,
				// ... named "GetKAID"
				"GetKAID",
				types.NewSignature(
					// ... that doesn't have a receiver
					nil,
					// ... or any params
					nil,
					// ... and returns a string
					types.NewTuple(
						types.NewVar(
							token.NoPos,
							nil,
							"",
							types.Universe.Lookup("string").Type(),
						),
					),
					// ... that isn't variadic.
					false,
				),
			),
		},
		// (and there are no interface embeds)
		nil,
	)
	kaiderInterface.Complete()

	// The strategy for this linter is as follows:
	//
	// 1. Look for function declarations
	// 2. If the declaration is a method declaration
	// 3. And the method name is "TransactionSafetyPolicy"
	// 4. Look at the return expression
	// 5. If the expression is a selector (e.g. "datastore.<something>")
	// 6. And the selector name is "WrittenWithUserLockModel"
	// 7. Verify that the receiver type of the "TransactionSafetyPolicy" method
	//       has a GetKAID() method (by verifying that it implements the
	//       interface defined above).
	for _, file := range pass.Files {
		for _, decl := range file.Decls {
			// 1
			funcDecl, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}

			// 2
			if funcDecl.Recv == nil {
				continue
			}

			// 3
			if funcDecl.Name.Name != "TransactionSafetyPolicy" {
				continue
			}

			// 4
			var policyReturnExpr ast.Expr
			ast.Inspect(funcDecl.Body, func(node ast.Node) bool {
				// We're looking for "return <something>".
				var returnStmt *ast.ReturnStmt
				if returnStmt, ok = node.(*ast.ReturnStmt); ok {
					if len(returnStmt.Results) == 1 {
						policyReturnExpr = returnStmt.Results[0]
					}
				}

				return true // always recurse (this function should be very short)
			})
			if policyReturnExpr == nil {
				continue
			}

			// 5
			modelPolicySelector, ok := policyReturnExpr.(*ast.SelectorExpr)
			if !ok {
				continue
			}

			// 6
			isUserLockModel := modelPolicySelector.Sel.Name == "WrittenWithUserLockModel"
			if !isUserLockModel {
				continue
			}

			// 7
			recvExpr := funcDecl.Recv.List[0].Type
			recvType := pass.TypesInfo.TypeOf(recvExpr).Underlying()
			if !types.Implements(recvType, kaiderInterface) {
				pass.Reportf(modelPolicySelector.Pos(),
					"WrittenWithUserLockModel model must have a GetKAID method")
			}
		}
	}

	return nil, nil
}
