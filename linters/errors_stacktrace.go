package linters

// This file contains static analysis of errors and make sure they are called
// with errors.Wrap appropriately to ensure stack traces are accurate. Broadly
// speaking there are 2 cases where this is important.
// 1. The error comes from an external codebase like io or json. The errors
//    coming from these functions are just plain vanilla old errors and not
//    fancy khanErrors so we errors.Wrap() them to get our fancy features
//    like stack traces.
// 2. The error is declared as a sentinel. Sentinel errors can be declared in
//    in the file same file it's used in, or in another file and exported.
//    In these cases the stack traces will point to where they're declared
//    at file scope and not the important place of where they came from.
//    These are of course discovered separately but the treatment is the
//    same: wrap them.
import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"io/ioutil"
	"os"
	"strings"

	"golang.org/x/tools/go/analysis"

	"github.com/StevenACoffman/fixer/lintutil"
)

var ErrorsWrapStacktraceAnalyzer = &analysis.Analyzer{
	Name: "errors_stacktrace",
	Doc:  "verifies errors.Wrap() is called in cases where stack trace is missing or incorrect",
	Run:  _runErrorsWrapStacktraceCorrect,
}

func _isASentinel(obj types.Object, sentinels []types.Object) bool {
	for _, sentinel := range sentinels {
		if sentinel == obj {
			return true
		}
	}
	return false
}

func _handleCallExpr(pass *analysis.Pass, callExpr *ast.CallExpr) {
	funcObj := lintutil.ObjectFor(callExpr.Fun, pass.TypesInfo)
	funcName := lintutil.NameOf(funcObj)

	if _callExprErrorsRequireWrapping(pass, callExpr) {
		// We don't fix these because some calls are complicated looking
		// structures and dumping them into a errors.Wrap() would just make
		// it weirder looking. These happen fairly seldomly, so it's
		// probably ok to expect the developer to decide what to do with
		// these.
		pass.Reportf(callExpr.Pos(), fmt.Sprintf("Calls to external functions "+
			"like %v that return errors need to be wrapped with errors.Wrap",
			funcName))
	}
}

var fileContents = make(map[string][]string)

func _getLineOfText(file *token.File, lineNumber int) (string, error) {
	var lines []string
	var ok bool
	if lines, ok = fileContents[file.Name()]; !ok {
		fp, err := os.Open(file.Name())
		if err != nil {
			return "", fmt.Errorf("%w", err)
		}
		defer fp.Close()

		contents, err := ioutil.ReadAll(fp)
		if err != nil {
			return "", fmt.Errorf("%w", err)
		}
		lines = strings.Split(string(contents), "\n")
		fileContents[file.Name()] = lines
	}
	if lineNumber > len(lines) {
		return "", fmt.Errorf("file is not long enough name: %v",
			file.Name())
	}

	return lines[lineNumber-1], nil
}

// Ok this is kind of ugly
func _reportWithEdit(pass *analysis.Pass, file *token.File, ident *ast.Ident, message string) {
	lineNumber := file.Line(ident.Pos())
	line, err := _getLineOfText(file, lineNumber)
	if err != nil {
		// err should be nil unless something is very odd is afoot.
		// It only happens if the file we're linting doesn't exist, or if
		// the number of lines have changed while linting.
		panic(err)
	}

	// From pkg/golinters/goanalysis/linter.go:269
	// we see that we can only replace WHOLE lines. We only care about
	// making a replacement mid-line, so that's easier to do here.
	// we segment the lint into 3 chunks:
	// * start - startOffset (which we call the prefix of the line)
	// * middle - startOffset to endOffset (mid)
	// * end - endOffset to end of the line (suffix)
	// The middle should be the thing we want wrapped, so the new line is just
	// prefix + "errors.Wrap(" + mid + ")" + suffix + "\n"
	// The newline character is important because without it golangcli-lint
	// will ignore all this work and not tell you why.
	startOfLine := file.LineStart(lineNumber)
	endOfLine := file.LineStart(lineNumber + 1)
	startOffset := ident.Pos() - startOfLine
	endOffset := ident.End() - startOfLine
	prefix := line[:startOffset]
	mid := line[startOffset:endOffset]
	suffix := line[endOffset:]

	pass.Report(analysis.Diagnostic{
		Pos:     ident.Pos(),
		Message: message,
		SuggestedFixes: []analysis.SuggestedFix{{
			TextEdits: []analysis.TextEdit{
				{
					Pos:     startOfLine,
					End:     endOfLine,
					NewText: []byte(prefix + "errors.Wrap(" + mid + ")" + suffix + "\n"),
				},
			},
		}},
	})
}

func _handleErrorIdent(
	pass *analysis.Pass,
	file *token.File,
	errReturn *ast.Ident,
	sentinels []types.Object,
	requiresWrapping map[types.Object]token.Pos,
) {
	obj := lintutil.ObjectFor(errReturn, pass.TypesInfo)
	if ast.IsExported(lintutil.NameOf(obj)) {
		_reportWithEdit(pass, file, errReturn, "You must wrap errors that are exported with errors.Wrap() before you return them.")
	} else if _, found := requiresWrapping[obj]; found {
		_reportWithEdit(pass, file, errReturn, "You must wrap errors that are from non-KA code with errors.Wrap() before you return them.")
	} else if _isASentinel(obj, sentinels) {
		_reportWithEdit(pass, file, errReturn, "You must wrap errors that are sentinels with errors.Wrap() before you return them.")
	}
}

// funcNames that contain github.com/Khan/webapp/ are in our code base so
// we assume they return khanErrors. fmt.Errorf are handled by the banned
// symbol linter so we ignore those assuming they're handled correctly
// already. Finally some code looks like production code, but really deals
// with mocks. We don't care about mock errors. All other errors are fair
// game and should be wrapped.
func _callExprErrorsRequireWrapping(pass *analysis.Pass, caller *ast.CallExpr) bool {
	funcObj := lintutil.ObjectFor(caller.Fun, pass.TypesInfo)
	funcName := lintutil.NameOf(funcObj)
	return !strings.Contains(funcName, "github.com/Khan/webapp/") &&
		funcName != "fmt.Errorf" &&
		funcName != "(github.com/stretchr/testify/mock.Arguments).Error"
}

// We look through assignments within a function to see if any of the variables
// are errors coming from external codebases.
func _handleAssignment(
	pass *analysis.Pass,
	assign *ast.AssignStmt,
	requiresWrapping map[types.Object]token.Pos,
) {
	for i, expr := range assign.Lhs {
		argType := pass.TypesInfo.TypeOf(expr)
		if argType != nil && argType.String() == "error" {
			var caller *ast.CallExpr
			var ok bool
			if len(assign.Lhs) == len(assign.Rhs) {
				caller, ok = assign.Rhs[i].(*ast.CallExpr)
			} else {
				caller, ok = assign.Rhs[0].(*ast.CallExpr)
			}
			if ok {
				if _callExprErrorsRequireWrapping(pass, caller) {
					requiresWrapping[lintutil.ObjectFor(expr, pass.TypesInfo)] = expr.Pos()
				} else {
					delete(requiresWrapping, lintutil.ObjectFor(expr, pass.TypesInfo))
				}
			}
		}
	}
}

func _handleReturn(
	pass *analysis.Pass,
	file *token.File,
	ret *ast.ReturnStmt,
	sentinels []types.Object,
	requiresWrapping map[types.Object]token.Pos,
) {
	for _, errReturn := range ret.Results {
		argType := pass.TypesInfo.TypeOf(errReturn)
		if argType.String() != "error" {
			continue
		}

		switch ret := errReturn.(type) {
		case *ast.Ident:
			_handleErrorIdent(pass, file, ret, sentinels, requiresWrapping)
		case *ast.CallExpr:
			_handleCallExpr(pass, ret)
		case *ast.SelectorExpr, *ast.StarExpr:
			// TODO(jeremygervais): Handle case where we return *foo.error,
			// foo.error - selector, where foo is a object in a non-khan
			// code base
			continue
		case *ast.IndexExpr:
			continue // This is a case like return errors[1] which happens
			// rarely so it's probably ok to ignore for now.
		}
	}
}

func _checkFunctionDeclaration(
	pass *analysis.Pass,
	file *token.File,
	node ast.Node,
	sentinels []types.Object,
) {
	// As we walk through the function, we keep track of all the objects
	// declared and assigned in it. Each object that is created through a
	// external function, if returned, will report an error if it's not wrapped
	// first.
	errorsRequiredWrapping := make(map[types.Object]token.Pos)
	ast.Inspect(node, func(node ast.Node) bool {
		// Inside of a function, we care about errors in two ways, where we
		// return them and where we assign them.
		// Returns matter because you can do:
		// ```return foo.getSomeError()```
		// and that function should be wrapped, or the code can say something
		// like:
		// ```
		// err := foo.getSomeError()
		// return err
		// ```
		// where we need to get that function call wrapped on the assignment.
		switch stmt := node.(type) {
		case *ast.ReturnStmt:
			_handleReturn(pass, file, stmt, sentinels, errorsRequiredWrapping)
		case *ast.AssignStmt:
			_handleAssignment(pass, stmt, errorsRequiredWrapping)
		}
		return true
	})
}

// We look through the file and find all the top level declerations
// of error variables. We use this list later on in the code to see
// if we are returning a bare sentinel without wrapping.
func getSentinels(pass *analysis.Pass, file *ast.File) []types.Object {
	sentinels := make([]types.Object, 0)
	for _, decl := range file.Decls {
		genDecl, ok := decl.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, spec := range genDecl.Specs {
			valueSpec, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for _, name := range valueSpec.Names {
				tName := pass.TypesInfo.TypeOf(name)
				if tName.String() == "error" {
					sentinels = append(sentinels, lintutil.ObjectFor(name, pass.TypesInfo))
				}
			}
		}
	}
	return sentinels
}

func _runErrorsWrapStacktraceCorrect(pass *analysis.Pass) (interface{}, error) {
	for _, file := range pass.Files {
		// First we scan the file looking for sentinel errors which may be
		// referenced in declared functions.
		sentinels := getSentinels(pass, file)

		for _, decl := range file.Decls {
			switch node := decl.(type) {
			case *ast.FuncDecl:
				_checkFunctionDeclaration(pass, pass.Fset.File(file.Pos()), decl, sentinels)
			case *ast.GenDecl:
				for _, spec := range node.Specs {
					if valspec, ok := spec.(*ast.ValueSpec); ok {
						if _, isFuncLit := valspec.Type.(*ast.FuncLit); isFuncLit {
							_checkFunctionDeclaration(
								pass, pass.Fset.File(file.Pos()), valspec.Type, sentinels)
						}
					}
				}
			}
		}
	}
	return nil, nil
}
