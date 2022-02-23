### Fixer

golangci-lint does not support applying suggested fixes per https://github.com/golangci/golangci-lint/issues/1779

So this just does that for us.

Current golangci-linters that support `SuggestedFixes`:
- `goerr113` https://github.com/Djarvur/go-err113
- `ruleguard` https://github.com/quasilyte/go-ruleguard
- `nlreturn` https://github.com/ssgreg/nlreturn
- `exportloopref`https://github.com/kyoh86/exportloopref
- `exhaustive` https://github.com/nishanths/exhaustive
- `govet`
- `staticcheck`

There's also a few in golang.org/x/tools/go/analysis:

+ assign - Package assign defines an Analyzer that detects useless assignments.
+ fieldaligment - Package fieldalignment defines an Analyzer that detects structs that would use less memory if their fields were sorted.
+ sigchanyzer - check for unbuffered channel of os.Signal
+ sortslice - Package sortslice defines an Analyzer that checks for calls to sort.Slice that do not use a slice type as first argument.
+ stringintconv - Package stringintconv defines an Analyzer that flags type conversions from integers to strings.
+ unreachable - Package unreachable defines an Analyzer that checks for unreachable code.

Currently, this skips `findcall` and `rulesguard` which more fiddling to get working.

