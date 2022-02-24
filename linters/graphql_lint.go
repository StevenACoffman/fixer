package linters

// This file contains static analysis of GraphQL operations.  It exports two
// analyzers: one, GraphQLAnalyzer, which analyzes the operations (also used by
// the tool which extracts their operation-names), and one,
// GraphQLLintAnalyzer, which reports errors.

import (
	"fmt"
	"go/ast"
	"go/constant"
	"reflect"
	"strings"

	"golang.org/x/tools/go/analysis"

	"github.com/StevenACoffman/fixer/lintutil"
)

var GraphQLAnalyzer = &analysis.Analyzer{
	Name: "graphql",
	Doc:  "analyzes GraphQL operations",
	Run:  _runGraphQL,
	// This analyzer does not report errors: instead it just returns
	// information about each operation.  The below GraphQLLintAnalyzer reports
	// errors; the extract-go-graphql tool exports other data.
	ResultType: reflect.TypeOf([]GraphQLOperation(nil)),
}

// GraphQLOperation represents a GraphQL operation in the source code.
type GraphQLOperation struct {
	// Call the ast-node representing the function-call that makes this
	// operation.
	Call *ast.CallExpr
	// Error is a string, set if this operation could not be fully analyzed
	// (perhaps because it's wrong, or perhaps just because it's a bit too
	// dynamic for our static-analysis).
	Error string
	// OpName is the operation-name of the operation.
	OpName string
}

func (op GraphQLOperation) String() string {
	if op.Error != "" {
		return "error: " + op.Error
	}

	return "opname: " + op.OpName
}

// Names of functions that make a GraphQL operation (names as defined by
// lintutil.NameOf), mapped to the index of the argument that has the opname.
//
// These are also used by banned_symbol_lint.go.
var graphqlFunctions = map[string]int{
	"(github.com/Khan/webapp/pkg/web/gqlclient.Client).Query":              2,
	"(github.com/Khan/webapp/pkg/web/gqlclient.Client).ServiceAdminQuery":  2,
	"(github.com/Khan/webapp/pkg/web/gqlclient.Client).Mutate":             2,
	"(github.com/Khan/webapp/pkg/web/gqlclient.Client).ServiceAdminMutate": 2,
}

func _runGraphQL(pass *analysis.Pass) (interface{}, error) {
	var retval []GraphQLOperation

	for _, file := range pass.Files {
		// We don't care about operations in tests.
		filename := pass.Fset.File(file.Pos()).Name()
		if strings.HasSuffix(filename, "_test.go") {
			continue
		}

		ast.Inspect(file, func(node ast.Node) bool {
			// Look for calls...
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true // recurse
			}

			// ... to one of the functions we're interested in
			fnObj := lintutil.ObjectFor(call.Fun, pass.TypesInfo)
			argIndex, ok := graphqlFunctions[lintutil.NameOf(fnObj)]
			if !ok {
				return true
			}

			if argIndex >= len(call.Args) {
				retval = append(retval, GraphQLOperation{
					Call: call,
					Error: fmt.Sprintf(
						"error: unable to get GraphQL opname: only %v arguments found",
						len(call.Args)),
				})

				return true
			}

			// Report the opname.
			opNameArg := call.Args[argIndex]
			opNameValue := pass.TypesInfo.Types[opNameArg].Value
			switch {
			case opNameValue == nil:
				retval = append(retval, GraphQLOperation{
					Call:  call,
					Error: "unable to get GraphQL opname: non-constant argument",
				})
			case opNameValue.Kind() != constant.String:
				retval = append(retval, GraphQLOperation{
					Call:  call,
					Error: "unable to get GraphQL opname: non-string argument",
				})
			default:
				retval = append(retval, GraphQLOperation{
					Call:   call,
					OpName: constant.StringVal(opNameValue),
				})
			}

			// TODO(benkraft): Also extract the full text of the
			// operation-document we will send to the server.  This is somewhat
			// difficult because we need to use the GraphQL client to generate
			// it from the given type.  (And that's further complicated by the
			// fact that static analyses have access to go/types-style
			// descriptions of types, whereas the GraphQL client wants a
			// reflect-style description; those are two parallel but separate
			// worlds!)

			return true
		})
	}

	return retval, nil
}

var GraphQLLintAnalyzer = &analysis.Analyzer{
	Name:     "graphql_lint",
	Doc:      "reports on GraphQL operations that don't look right",
	Run:      _runGraphQLLint,
	Requires: []*analysis.Analyzer{GraphQLAnalyzer},
}

func _runGraphQLLint(pass *analysis.Pass) (interface{}, error) {
	// GraphQLAnalyzer already did all the work, we just have to report the
	// errors.
	ops, _ := pass.ResultOf[GraphQLAnalyzer].([]GraphQLOperation)
	for _, op := range ops {
		if op.Error != "" {
			pass.Report(analysis.Diagnostic{Pos: op.Call.Pos(), Message: op.Error})
		}
	}

	return nil, nil
}

var GraphQLTestAnalyzer = &analysis.Analyzer{
	Name:     "graphql_test",
	Doc:      "exports all data from GraphQLAnalyzer for tests",
	Run:      _runGraphQLTest,
	Requires: []*analysis.Analyzer{GraphQLAnalyzer},
}

func _runGraphQLTest(pass *analysis.Pass) (interface{}, error) {
	ops, _ := pass.ResultOf[GraphQLAnalyzer].([]GraphQLOperation)
	for _, op := range ops {
		// For tests, we export everything so we can test it all at once.
		pass.Report(analysis.Diagnostic{Pos: op.Call.Pos(), Message: op.String()})
	}

	return nil, nil
}
