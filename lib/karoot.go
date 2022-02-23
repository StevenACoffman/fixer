package lib

import (
	"context"
	"os"
	"path/filepath"
)

func KARoot(_ context.Context) string {
	// To find the KA-root, we look in the ancestors of the current
	// working-directory and then of the path of this file as of
	// compile time.
	//
	// In prod, the former is correct: PWD seems to always be the directory
	// with the app.yaml, which per deploy/deploy_go_service.py is always
	// webapp-root.  PWD is also good for tools: if the user is in a webapp
	// checkout, we'll use it, even if this tool was compiled in some other
	// webapp checkout somehow.
	//
	// The latter is correct in most non-prod environments: as long as you're
	// on the same machine that compiled this binary, and you haven't moved or
	// deleted the source from which it was compiled, it should work.  This
	// makes it so you can run webapp tools from outside webapp.
	//
	// If neither of those work, we're pretty screwed: you may not even have a
	// checkout of webapp available.  But that shouldn't really happen in any
	// of our supported use cases.
	cwd, err := os.Getwd()
	if err != nil {
		cwd = os.Getenv("PWD") // not as reliable, but can't error!
	}
	maybeRoot := maybeKARootAncestor(cwd)
	if maybeRoot != "" {
		return maybeRoot
	}
	home, err := os.UserHomeDir()
	if err == nil {
		return filepath.Join(home, "khan", "webapp")
	}
	panic(err)
}

// maybeKARootAncestor walks the ancestors of the given abspath, and returns
// the first one which looks like the root of the webapp repo (detected by the
// magic file .ka_root); if none do (including if the abspath doesn't exist at
// all, which may happen if this binary was built on a different system, as is
// the case in prod) it returns "".
func maybeKARootAncestor(abspath string) string {
	for abspath != "" && abspath != "/" {
		_, err := os.Stat(filepath.Join(abspath, ".ka_root"))
		if err == nil {
			return abspath
		}

		abspath = filepath.Dir(abspath)
	}

	return ""
}

// KARootJoin converts a path relative to the webapp repo root to be absolute.
func KARootJoin(ctx context.Context, elem ...string) string {
	// If the input is already an abspath, KARootJoin is a noop.
	if len(elem) > 0 && filepath.IsAbs(elem[0]) {
		return filepath.Join(elem...)
	}
	return filepath.Join(append([]string{KARoot(ctx)}, elem...)...)
}
