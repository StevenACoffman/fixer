package main

import (
	"github.com/StevenACoffman/fixer/linters"
	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/multichecker"
	"golang.org/x/tools/go/analysis/passes/fieldalignment"
	"golang.org/x/tools/go/analysis/passes/sigchanyzer"
	"honnef.co/go/tools/quickfix"
	"os"
	"strings"

	// Vet checks.
	"golang.org/x/tools/go/analysis/passes/asmdecl"
	"golang.org/x/tools/go/analysis/passes/assign"
	"golang.org/x/tools/go/analysis/passes/atomic"
	"golang.org/x/tools/go/analysis/passes/bools"
	"golang.org/x/tools/go/analysis/passes/buildtag"
	"golang.org/x/tools/go/analysis/passes/cgocall"
	"golang.org/x/tools/go/analysis/passes/copylock"
	"golang.org/x/tools/go/analysis/passes/errorsas"
	"golang.org/x/tools/go/analysis/passes/httpresponse"
	"golang.org/x/tools/go/analysis/passes/loopclosure"
	"golang.org/x/tools/go/analysis/passes/lostcancel"
	"golang.org/x/tools/go/analysis/passes/nilfunc"
	"golang.org/x/tools/go/analysis/passes/printf"
	"golang.org/x/tools/go/analysis/passes/shift"
	"golang.org/x/tools/go/analysis/passes/stdmethods"
	"golang.org/x/tools/go/analysis/passes/structtag"
	"golang.org/x/tools/go/analysis/passes/tests"
	"golang.org/x/tools/go/analysis/passes/unmarshal"
	"golang.org/x/tools/go/analysis/passes/unreachable"
	"golang.org/x/tools/go/analysis/passes/unsafeptr"
	"golang.org/x/tools/go/analysis/passes/unusedresult"

	// Additional checks in x/tools
	"golang.org/x/tools/go/analysis/passes/atomicalign"
	"golang.org/x/tools/go/analysis/passes/deepequalerrors"
	"golang.org/x/tools/go/analysis/passes/ifaceassert"
	"golang.org/x/tools/go/analysis/passes/nilness"
	"golang.org/x/tools/go/analysis/passes/sortslice"
	"golang.org/x/tools/go/analysis/passes/stringintconv"
	"golang.org/x/tools/go/analysis/passes/testinggoroutine"

	// Staticcheck
	"honnef.co/go/tools/config"
	"honnef.co/go/tools/simple"
	"honnef.co/go/tools/staticcheck"
	"honnef.co/go/tools/stylecheck"

	// One offs
	"github.com/Djarvur/go-err113"
	"github.com/kyoh86/exportloopref"
	"github.com/nishanths/exhaustive"
	// ruleguard "github.com/quasilyte/go-ruleguard/analyzer" // ruleguard needs a file or something?
	"github.com/ssgreg/nlreturn/v2/pkg/nlreturn"
)

func main() {

	var runKhan bool
	for _, a := range os.Args[1:] {
		if strings.HasPrefix(a, "-khan") {
			runKhan = true
		}
	}
	// Most of these linters do NOT have suggested fixes BTW.
	var checks = []*analysis.Analyzer{
		// All cmd/vet analyzers.
		asmdecl.Analyzer,
		assign.Analyzer,
		atomic.Analyzer,
		bools.Analyzer,
		buildtag.Analyzer,
		cgocall.Analyzer,
		// composite.Analyzer, // check for un-keyed composite literals
		copylock.Analyzer,
		errorsas.Analyzer,
		httpresponse.Analyzer,
		loopclosure.Analyzer,
		lostcancel.Analyzer,
		nilfunc.Analyzer,
		printf.Analyzer,
		shift.Analyzer,
		stdmethods.Analyzer,
		structtag.Analyzer,
		tests.Analyzer,
		unmarshal.Analyzer,
		unreachable.Analyzer,
		unsafeptr.Analyzer,
		unusedresult.Analyzer,

		// Additional checks from x/tools
		atomicalign.Analyzer,
		deepequalerrors.Analyzer,
		fieldalignment.Analyzer,
		ifaceassert.Analyzer,
		nilness.Analyzer,
		sigchanyzer.Analyzer,
		//shadow.Analyzer, // check for possible unintended shadowing of variables
		sortslice.Analyzer,
		stringintconv.Analyzer,
		testinggoroutine.Analyzer,

		// One Offs:
		nlreturn.NewAnalyzer(),
		err113.NewAnalyzer(),
		exportloopref.Analyzer,
		exhaustive.Analyzer,
		// ruleguard.Analyzer, // requires a dsl file
	}
	if runKhan {
		checks = append(checks, linters.ErrorsWrapStacktraceAnalyzer)
		checks = append(checks, linters.LinewrapAnalyzer)
	}
	config.DefaultConfig.Initialisms = append(config.DefaultConfig.Initialisms, "ISO")

	// Most of staticcheck.
	for _, v := range quickfix.Analyzers {
		checks = append(checks, v.Analyzer)
	}
	for _, v := range simple.Analyzers {
		checks = append(checks, v.Analyzer)
	}
	for _, v := range staticcheck.Analyzers {
		checks = append(checks, v.Analyzer)
	}
	for _, v := range stylecheck.Analyzers {
		// - At least one file in a non-main package should have a package comment
		// - The comment should be of the form "Package x ..."
		if v.Analyzer.Name == "ST1000" {
			continue
		}
		// The documentation of an exported function should start with
		// the function's name.
		if v.Analyzer.Name == "ST1020" {
			continue
		}
		// Skip for now due to bug in staticcheck in locations.go
		// TODO: lint:[..] directives don't seem to work. Actually,
		// staticcheck error codes are also ignored. Guess that's some frontend
		// it added on x/analysis?
		// TODO: send patch upstream.
		if v.Analyzer.Name == "ST1003" {
			continue
		}

		checks = append(checks, v.Analyzer)
	}

	// Add -fix unless already given.
	var has bool
	for _, a := range os.Args[1:] {
		if strings.HasPrefix(a, "-fix") {
			has = true

			break
		}
	}
	if !has {
		os.Args = append(
			[]string{os.Args[0], "-fix"},
			os.Args[1:]...)
	}

	multichecker.Main(checks...)
}
