package linters

// This linter replaces `lll` (the long-line linter) and adds auto-fixing.
//
// We are replacing `lll` to give it more flexibility.  For instance,
// we are ok with super-long lines for struct tags.  (Sometimes
// they're necessary since struct tags must be on the same line as the
// field they're annotating.)  The lll config doesn't support that,
// since you can't just use regexps to detect when a line is defining
// a struct field.
//
// We also replace `lll` so we can do some auto-fixing.  We don't
// auto-fix all long lines -- we're not trying to be prettier -- but
// we do fix some common cases:
//    1) Long lines in comment blocks
//    2) Long lines in function declarations

import (
	"fmt"
	"go/ast"
	"go/token"
	"io/ioutil"
	"os"
	"regexp"
	"strings"
	"unicode/utf8"

	"golang.org/x/tools/go/analysis"
)

const (
	gmaxCommentLineLen = 79
	gmaxCodeLineLen    = 100 // copied from `lll` linter
	gtabSpaces         = 4   // copied from `lll` linter
)

// We allow long lines that have any of these patterns.
// Note that this only applies to non-comment lines, comment
// lines have their own check in _isMachineReadableComment.
var gexceptionsRegexp = regexp.MustCompile(
	// Lines with a single string at the end (possibly followed by
	// punctuation), where the pre-string portion isn't too-long on
	// its own.
	`^[^"]{0,100}"(?:\\.|[^"])*"\W*$` +
		// Normally we don't allow a second string on the same line,
		// but we make an exception for maps with both string keys and
		// string values, because there's no good way to wrap map
		// key-value pairs.
		`|^\s*"(?:\\.|[^"])*":\s*"(?:\\.|[^"])*",$` +
		// Heck, let's allow string keys and int values as well.
		`|^\s*"(?:\\.|[^"])*":\s*\d+,$` +
		// genqlient makes really long symbol names, which we alias
		// to something short but need a long line to do it.
		`|^(?:type\s+)?\s*\w+\s*=\s*genqlient\.\w+$`,
)

var LinewrapAnalyzer = &analysis.Analyzer{
	Name: "linewrap",
	Doc: "check that comment lines fit in 80 chars, and func decls in 100. " +
		"Auto-fixing will rewrap both types of lines to fit.",
	Run: _runLinewrap,
}

// A convenient wrapper around token.File that also gives access to ast.File.
type _file struct {
	*token.File
	AstFile  *ast.File
	contents string
	// TODO(csilvers): store line offsets rather than full lines
	lines     []string
	readLines bool
}

func (f *_file) cacheFile() error {
	if !f.readLines {
		fp, err := os.Open(f.Name())
		if err != nil {
			return fmt.Errorf("can't open file %s %w", f.Name(), err)
		}
		defer fp.Close()

		contents, err := ioutil.ReadAll(fp)
		if err != nil {
			return fmt.Errorf("%w", err)
		}
		f.contents = string(contents)
		f.lines = strings.Split(f.contents, "\n")
		f.readLines = true
	}

	return nil
}

// Range returns the bytes in f between [start, end).
func (f *_file) Range(start, end token.Pos) (string, error) {
	err := f.cacheFile()
	if err != nil {
		return "", fmt.Errorf("%w", err)
	}
	if f.Offset(start) < 0 || f.Offset(start) >= f.Size() {
		return "", fmt.Errorf(
			"file doesn't have enough chars size:%d start: %d filename: %s",
			f.Size(), start, f.Name())
	}

	return f.contents[f.Offset(start):f.Offset(end)], nil
}

func (f *_file) NumLines() (int, error) {
	err := f.cacheFile()
	if err != nil {
		return -1, fmt.Errorf("%w", err)
	}

	return len(f.lines), nil
}

// LineEnd points to the newline at the end of the line (or the end
// of the file if the file doesn't end in a newline).
func (f *_file) LineEnd(lineNumber int) token.Pos {
	if lineNumber+1 < len(f.lines)-1 {
		// If there's a next line, replace up to the start of that.
		return f.LineStart(lineNumber+1) - 1
	}
	return f.AstFile.End()
}

func (f *_file) LineText(lineNumber int) (string, error) {
	err := f.cacheFile()
	if err != nil {
		return "", fmt.Errorf("%w", err)
	}
	if lineNumber < 1 || lineNumber-1 >= len(f.lines) {
		return "", fmt.Errorf(
			"file doesn't have enough lines, lines: %d lineNumber: %d, fileName %s",
			len(f.lines), lineNumber, f.Name())
	}

	return f.lines[lineNumber-1], nil
}

func _lineLength(line string, tabSpaces int) int {
	// We count every tab as 4 runes, not 1.
	return utf8.RuneCountInString(line) +
		strings.Count(line, "\t")*(tabSpaces-1)
}

func _diagnostic(
	file *_file,
	startLine, endLine, errorLine int,
	replacement []string,
	message string,
) analysis.Diagnostic {
	errorPos := file.LineStart(errorLine)
	retval := analysis.Diagnostic{
		Pos:     errorPos,
		Message: message,
	}
	if len(replacement) > 0 {
		startPos := file.LineStart(startLine)
		endPos := file.LineEnd(endLine) + 1
		// We have to add trailing newlines to each replacement line.
		newText := []byte(strings.Join(replacement, "\n") + "\n")
		suggestedFix := analysis.SuggestedFix{
			Message: "Reflowed text",
			TextEdits: []analysis.TextEdit{
				{Pos: startPos, End: endPos, NewText: newText},
			},
		}
		retval.SuggestedFixes = []analysis.SuggestedFix{suggestedFix}
	}

	return retval
}

// Allows a line that's a url all by itself, or `// [1] <url>`.
var _urlComment = regexp.MustCompile(
	`^\s*//(?:\s*\[\d+\]:?)?\s*(?:https?|gs|web\+graphie)://\S*$`)

// Return whether a comment-only line (that is, a line that is only
// comments) is a human-readable comment, which we can always wrap,
// vs. a machine-read comment, which we often can't.
func _isMachineReadableComment(line string) bool {
	normline := strings.TrimSpace(line)
	// The space is important: we ignore machine-readable comments.
	return strings.HasPrefix(normline, "//") &&
		(!strings.HasPrefix(normline, "// ") ||
			// Another type of machine directive.
			strings.HasPrefix(normline, "// +build") ||
			// Our sync-tags don't follow the Go convention that
			// machine-readable comments have no space after the `//`.
			strings.HasPrefix(normline, "// sync-"))
}

// For a comment line, all the text up until the actual comment: "\t\t// "
// TODO(csilvers): find all unicode whitespace, not just space and tab
func _commentPrefix(line string) string {
	commentStart := strings.Index(line, "// ")
	if commentStart != -1 {
		commentStart += len("//")
	} else {
		commentStart = strings.Index(line, "/*")
		if commentStart != -1 { // C-style comment
			commentStart += len("/*")
		} else {
			commentStart = 0 // continuation line in C-style-comment
		}
	}

	for commentStart < len(line) &&
		(line[commentStart] == ' ' || line[commentStart] == '\t') {
		commentStart++
	}

	return line[:commentStart]
}

// shareCommentBlock is true if two comments are in the same "comment
// block."  Comment blocks are denoted by the comment being indented
// the same amount, and having the same indentation *after* the
// comment-start.  I also look for some ascii-art list starters,
// and if `line` has that it's its own comment block.
//    In comment block 1.
//    Still In comment block 1.
//    * Now in comment block 2
//    * Now in comment block 3
//       func inCommentBlock4() {
//          inCommentBlock5;
//       } // comment block 6
//    Now we are in comment block 7.
//    Where we will stay for a few lines.
//    TODO(benkraft): This is now comment block 8.
// If either line is a machine-readable comment, or a url, it is not
// shareable with any other line.
func _shareCommentBlock(line, otherLine string) bool {
	if _isMachineReadableComment(line) || _isMachineReadableComment(otherLine) {
		return false
	}
	if _urlComment.MatchString(line) || _urlComment.MatchString(otherLine) {
		return false
	}

	linePrefix := _commentPrefix(line)
	commentText := line[len(linePrefix):]
	if strings.HasPrefix(commentText, "* ") ||
		strings.HasPrefix(commentText, ". ") ||
		strings.HasPrefix(commentText, "- ") ||
		strings.HasPrefix(commentText, "TODO") ||
		strings.HasPrefix(strings.TrimLeft(commentText, "0123456789"), ") ") {
		return false
	}

	return linePrefix == _commentPrefix(otherLine)
}

func _linewrapComments(lines []string, maxCommentLineLen, tabSpaces int) []string {
	prefix := _commentPrefix(lines[0])

	// If we're reformatting anyway, let's give a bit of space on the
	// right margin so the comments don't look very crowded.
	maxCommentLineLen = maxCommentLineLen * 95 / 100

	retval := []string{prefix}

	for _, line := range lines {
		// Get rid of the comment prefix, but make sure every word
		// ends with a space.
		line = line[len(prefix):] + " "
		// We don't split on tabs, just spaces.
		for _, word := range strings.SplitAfter(line, " ") {
			// In some cases we can end up with empty words; just ignore those.
			// TODO(benkraft): Can we get SplitAfter to omit these?
			if word == "" {
				continue
			}
			lineLen := _lineLength(retval[len(retval)-1], tabSpaces)
			// We have a `-1` here because if we're the last word on
			// the line our trailing space will be deleted below.
			if lineLen+_lineLength(word, tabSpaces)-1 > maxCommentLineLen {
				retval = append(retval, prefix) // start a new line
			}
			retval[len(retval)-1] += word
		}
	}

	// The above left a trailing space on each line.  Remove it.
	for i, line := range retval {
		retval[i] = strings.TrimRight(line, " ")
	}

	return retval
}

func _getCommentBlockIssue(
	file *_file,
	startLine int,
	lines []string,
	maxCommentLineLen, tabSpaces int,
) []analysis.Diagnostic {
	// If this block consists only of a single, machine-readable
	// comment, then skip it; we can't wrap machine-read code.  (And
	// `_shareCommentBlock` ensures we'll never see a machine-readable
	// comment-line in the same block as any other comment-line.)
	if len(lines) == 1 && _isMachineReadableComment(lines[0]) {
		return nil
	}

	for i, line := range lines {
		lineLen := _lineLength(line, tabSpaces)

		// Look for lines that are too-long (and aren't just a URL)
		if lineLen > maxCommentLineLen && !_urlComment.MatchString(line) {
			message := fmt.Sprintf("comment line is %d characters", lineLen)
			replacement := _linewrapComments(lines, maxCommentLineLen, tabSpaces)
			diagnostic := _diagnostic(
				file, startLine, startLine+len(lines)-1, startLine+i,
				replacement, message)

			return []analysis.Diagnostic{diagnostic}
		}
	}

	return nil
}

// _getCommentIssuesForFile updates lintedLines in place.
func _getCommentIssuesForFile(
	file *_file,
	maxCommentLineLen, tabSpaces int,
	lintedLines map[int]bool,
) ([]analysis.Diagnostic, error) {
	var diagnostics []analysis.Diagnostic
	var err error

	for _, commentGroup := range file.AstFile.Comments {
		startLine := file.Line(commentGroup.Pos())
		endLine := file.Line(commentGroup.End())

		commentLines := make([]string, endLine-startLine+1)
		for i := 0; i < len(commentLines); i++ {
			commentLines[i], err = file.LineText(startLine + i)
			if err != nil {
				return diagnostics, fmt.Errorf("%w", err)
			}

			// For end-of-line comments, Go seems to create a one-entry
			// comment-group.  Ignore that here; we are only interested in
			// full-line comments (lines with just comments and no code).
			if len(commentLines) == 1 &&
				!strings.HasPrefix(strings.TrimSpace(commentLines[0]), "//") {
				commentLines = nil

				break
			}

			lintedLines[startLine+i] = true
		}

		if commentLines == nil {
			continue
		}

		// Now break up this comment-group into "blocks".  We consider
		// a single group of comments to potentially be multiple
		// blocks if it has, e.g., a list in it.
		blockStarts := []int{0}
		lastBlockStartLine := commentLines[0]

		for i := 1; i < len(commentLines); i++ {
			if !_shareCommentBlock(commentLines[i], lastBlockStartLine) {
				blockStarts = append(blockStarts, i)
				lastBlockStartLine = commentLines[i]
			}
		}
		blockStarts = append(blockStarts, len(commentLines)) // sentinel

		// Finally, analyze and linewrap each block separately.
		for i := 0; i < len(blockStarts)-1; i++ {
			block := commentLines[blockStarts[i]:blockStarts[i+1]]
			blockIssues := _getCommentBlockIssue(
				file, startLine+blockStarts[i], block, maxCommentLineLen, tabSpaces)
			diagnostics = append(diagnostics, blockIssues...)
		}
	}

	return diagnostics, nil
}

// lintedLine is set to the line-number of this funcDecl line if we
// successfully linted it (it's always one-line func decl in that
// case), or -1 if not.
func _getFuncIssue(
	file *_file,
	funcDecl *ast.FuncDecl,
	maxLineLen, tabSpaces int,
) (diagnostics []analysis.Diagnostic, lintedLine int, err error) {
	startLine := file.Line(funcDecl.Pos())
	// This is the end of the decl: the pos of the `{` that starts the body
	endLine := file.Line(funcDecl.Body.Pos())

	line, err := file.LineText(startLine)
	if err != nil {
		return nil, -1, fmt.Errorf("%w", err)
	}

	lineLen := _lineLength(line, tabSpaces)
	if lineLen <= maxLineLen {
		return nil, startLine, nil
	}

	if endLine > startLine {
		// We don't try to auto-wrap multi-line function declarations.
		// Return `-1` so this line will get linted again (normally) later.
		return nil, -1, nil
	}

	params := funcDecl.Type.Params
	if len(params.List) == 0 {
		// If there are no params our auto-wrapping may not look
		// very nice (and would require special handling where we index
		// params.List below).  So for now, we don't try to suggest there.
		return nil, -1, nil
	}

	// Now let's do the auto-reformatting.
	//
	// We only want to replace the function params, not the parens that
	// surround them, so we start with params.List[0].Pos() instead of
	// params.Pos().
	firstLine, err := file.Range(funcDecl.Pos(), params.List[0].Pos())
	if err != nil {
		return nil, -1, fmt.Errorf("%w", err)
	}
	newLines := []string{firstLine}

	for i, param := range params.List {
		// "gofmt" doesn't allow comments before a parameter
		// declaration, but it does allow them after.  We have to make
		// sure we include the latter.
		paramStart := param.Pos()
		paramEnd := param.End()
		if param.Comment != nil {
			paramEnd = param.Comment.End()
		}
		// TODO(csilvers): param.Comment seems to always be empty!
		// We can't fix this in general, but we can for the last
		// param in the argument list.  It's grody though.
		if i == len(params.List)-1 {
			paramEnd = params.Closing
		}

		// We need to insert a trailing comma, but gofmt wants it
		// before any trailing comments that follow the param.
		lineText, paramErr := file.Range(paramStart, param.End())
		if paramErr != nil {
			return nil, -1, fmt.Errorf("%w", paramErr)
		}
		lineText += ","
		if param.End() != paramEnd {
			commentText, paramErr := file.Range(param.End(), paramEnd)
			if paramErr != nil {
				return nil, -1, fmt.Errorf("%w", paramErr)
			}
			lineText += commentText
		}

		newLines = append(newLines, "\t"+lineText)
	}

	// TODO(csilvers): also reformat the return types if they are too
	// long.  (Useful for this is params.Closing and
	// funcDecl.Body.Lbrace.)  But for now we take everything from the
	// closing paren of the function declaration to the end of the
	// line, and put it in as our last-line.  Maybe we should also have a
	// TODO(csilvers): break up `func foo(...) { on same line }`.
	lastLine, err := file.Range(params.Closing, file.LineEnd(endLine))
	if err != nil {
		return nil, -1, fmt.Errorf("%w", err)
	}
	newLines = append(newLines, lastLine)

	msg := fmt.Sprintf("function line is %d characters", lineLen)
	diagnostic := _diagnostic(file, startLine, startLine, startLine, newLines, msg)

	return []analysis.Diagnostic{diagnostic}, startLine, nil
}

// _getFuncIssuesForFile updates lintedLines in place.
func _getFuncIssuesForFile(
	file *_file,
	maxLineLen, tabSpaces int,
	lintedLines map[int]bool,
) ([]analysis.Diagnostic, error) {
	var diagnostics []analysis.Diagnostic
	var err error

	for _, node := range file.AstFile.Decls {
		funcDecl, ok := node.(*ast.FuncDecl)
		if !ok {
			continue
		}

		funcIssues, lintedLine, thisErr := _getFuncIssue(file, funcDecl, maxLineLen, tabSpaces)
		if thisErr != nil {
			// We'll ignore this function declaration, but mark its error.
			err = thisErr

			continue
		}
		if lintedLine > -1 {
			lintedLines[lintedLine] = true
		}

		diagnostics = append(diagnostics, funcIssues...)
	}

	return diagnostics, err
}

// _findOKStructFields finds lines that are long just because of
// struct tags, e.g.:
//    struct MyStruct {
//       myfield `json:"foobarbang" graphql:"foobarbang" otherthing:....`
//    }
// Since struct tags have to be on the same line as the field, there's
// no way to wrap the long line, so we just allow it.  This function
// finds lines that are long just because of that, and marks them as
// "linted", without giving any warnings, so subsequent passes won't
// mark them as too-long.
func _findOKStructFields(file *_file, lintedLines map[int]bool) {
	ast.Inspect(file.AstFile, func(node ast.Node) bool {
		structField, ok := node.(*ast.Field)
		if !ok || structField.Tag == nil {
			// TODO(csilvers): are there some types we can short-circuit here?
			return true
		}
		// OK, we're a field with a tag.
		lineNum := file.Line(structField.Tag.ValuePos)

		// We'll just allow fields with tags to be arbitrarily long.
		// TODO(csilvers): validate that the field without the tag is <100 cols
		lintedLines[lineNum] = true

		return true
	})
}

// _findOKRawStrings finds lines that are long because the end with a
// raw-string.  Note we have a similar rule for normal strings, that
// is incorporated in gexceptionsRegexp.  But we can't do the same
// for raw-strings since they may span multiple lines, and gexceptionsRegexp
// only works a line at a time.  So we have to do it via code.
func _findOKRawStrings(file *_file, lintedLines map[int]bool) {
	ast.Inspect(file.AstFile, func(node ast.Node) bool {
		basicLit, ok := node.(*ast.BasicLit)
		if ok && basicLit.Kind == token.STRING &&
			len(basicLit.Value) > 0 && basicLit.Value[0] == '`' {
			// Mark the whole token-range as okay.
			// TODO(benkraft): Validate the start and end lines are <100 cols
			// without the string.
			startLineNum := file.Line(basicLit.Pos())
			endLineNum := file.Line(basicLit.End())
			for lineNum := startLineNum; lineNum <= endLineNum; lineNum++ {
				lintedLines[lineNum] = true
			}
		}

		return true // always recurse
	})
}

func _getLonglineIssuesForFile(
	file *_file,
	maxLineLen, tabSpaces int,
	lintedLines map[int]bool,
) ([]analysis.Diagnostic, error) {
	var diagnostics []analysis.Diagnostic
	var err error

	numLines, err := file.NumLines()
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}

	for i := 1; i <= numLines; i++ {
		if lintedLines[i] {
			continue
		}

		line, err := file.LineText(i)
		if err != nil {
			return nil, fmt.Errorf("%w", err)
		}

		if gexceptionsRegexp.MatchString(line) {
			continue
		}

		// We allow "nolint" directives to make a line be over-long
		// (since they're required to be on the same line as what they
		// modify).  But only if the line would have been short enough
		// otherwise.
		nolintStart := strings.Index(line, " //nolint:")
		if nolintStart > -1 {
			line = line[:nolintStart]
		}

		lineLen := _lineLength(line, tabSpaces)
		if lineLen > maxLineLen {
			msg := fmt.Sprintf("line is %d characters", lineLen)
			diagnostics = append(diagnostics, _diagnostic(file, i, i, i, nil, msg))
		}
	}

	return diagnostics, nil
}

func _runLinewrap(pass *analysis.Pass) (interface{}, error) {
	for _, f := range pass.Files {
		file := _file{File: pass.Fset.File(f.Pos()), AstFile: f}
		fmt.Println(file.Name())

		var diagnostics []analysis.Diagnostic
		lintedLines := make(map[int]bool)

		commentIssues, err := _getCommentIssuesForFile(
			&file, gmaxCommentLineLen, gtabSpaces, lintedLines)
		if err != nil {
			return nil, fmt.Errorf("%w", err)
		}
		diagnostics = append(diagnostics, commentIssues...)

		funcIssues, err := _getFuncIssuesForFile(
			&file, gmaxCodeLineLen, gtabSpaces, lintedLines)
		if err != nil {
			return nil, fmt.Errorf("%w", err)
		}
		diagnostics = append(diagnostics, funcIssues...)

		// Mark special cases that we are ok with.
		_findOKStructFields(&file, lintedLines)
		_findOKRawStrings(&file, lintedLines)

		// Now do the normal `lll` (too-long-line) linting.  We ignore
		// all line #s in lintedLines so they're not linted twice.
		longlineIssues, err := _getLonglineIssuesForFile(
			&file, gmaxCodeLineLen, gtabSpaces, lintedLines)
		if err != nil {
			return nil, fmt.Errorf("%w", err)
		}
		diagnostics = append(diagnostics, longlineIssues...)

		for _, diagnostic := range diagnostics {
			pass.Report(diagnostic)
		}
	}
	// The first return value here is available if we want to pass
	// data to other analyzers.  We don't use it at this time.
	return nil, nil
}
