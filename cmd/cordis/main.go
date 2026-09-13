// Command cordis inspects cordis-go configuration layers.
//
// It is the Go equivalent of `dsh --profile <name> --dump-config`: composition
// is pure data, so the dump needs no plugin registry and never runs plugin code.
//
//	cordis dump base.json                         # base layer only
//	cordis dump base.json profile.json            # profile.json patches base.json
//	cordis dump --strict base.json profile.json   # unmatched patch ids are errors
//	cordis dump --patch=base.json base.json       # force base.json to patch mode
//	cordis --help                                 # usage on stdout, exit 0
//
// The first file is the base layer and every later file is a patch layer applied
// in order, so the `.patch.json` suffix only changes how the first file is read.
// A file named by --patch=<path> is always parsed as a patch layer, wherever it
// appears: the explicit flag wins over both the position and the suffix.
//
// The exit status is part of the interface: 0 for success and explicit help, 1
// for a load or compose failure, 2 for a command line that cannot be understood.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/tr1v3r/cordis-go/loader"
)

// errHelp reports an explicit help request. The caller prints the usage text to
// stdout and exits 0 instead of treating it as a failure.
var errHelp = errors.New("help requested")

// usageError reports a command line that cannot be understood. The caller
// prints the message and the usage text to stderr and exits 2.
type usageError struct{ msg string }

// Error implements the error interface.
func (e *usageError) Error() string { return e.msg }

func usageErrorf(format string, args ...any) error {
	return &usageError{msg: fmt.Sprintf(format, args...)}
}

// isUsageError reports whether err is a command line error rather than a
// runtime failure.
func isUsageError(err error) bool {
	var target *usageError
	return errors.As(err, &target)
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run executes one command line and returns the process exit status. The
// streams are parameters so the exit status contract stays testable without a
// subprocess: out carries dumps and help, errOut carries diagnostics and the
// usage text printed after a usage error.
func run(args []string, out, errOut io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(errOut, "cordis: missing command")
		usage(errOut)
		return 2
	}
	switch args[0] {
	case "dump":
		err := dump(args[1:], out)
		switch {
		case err == nil:
			return 0
		case errors.Is(err, errHelp):
			usage(out)
			return 0
		case isUsageError(err):
			fmt.Fprintln(errOut, "cordis:", err)
			usage(errOut)
			return 2
		default:
			fmt.Fprintln(errOut, "cordis:", err)
			return 1
		}
	case "-h", "--help", "help":
		usage(out)
		return 0
	default:
		fmt.Fprintf(errOut, "cordis: unknown command %q\n", args[0])
		usage(errOut)
		return 2
	}
}

// usage writes the command line reference to w.
func usage(w io.Writer) {
	fmt.Fprint(w, `usage:
  cordis dump [--strict] [--patch=<file.json>]... <file.json>...

Compose configuration layers into a tree and print it with provenance comments.

  <file.json>          the first file is the base layer; every later file is a
                       patch layer applied in order, and a first file named
                       *.patch.json is a patch layer as well
  --patch=<file.json>  parse this file as a patch layer even when it is the
                       first file; may be repeated, and wins over the position
                       and the *.patch.json suffix
  --strict             fail on a patch id that matches no entry
  -h, --help           print this text and exit

Exit status: 0 on success or an explicit help request, 1 on a load or compose
failure, 2 on a usage error.
`)
}

// dump composes the layers named on the command line and writes the tree to w.
func dump(args []string, w io.Writer) error {
	options := []loader.ComposeOption{}
	forced := map[string]bool{}
	var paths []string
	for _, arg := range args {
		switch {
		case arg == "-h" || arg == "--help":
			return errHelp
		case arg == "--strict":
			options = append(options, loader.Strict())
		case strings.HasPrefix(arg, "--patch="):
			path := strings.TrimPrefix(arg, "--patch=")
			if path == "" {
				return usageErrorf("--patch requires a non-empty path")
			}
			forced[path] = true
		case arg == "--patch":
			return usageErrorf("--patch requires a path: write --patch=<file.json>")
		case strings.HasPrefix(arg, "-"):
			// A single-dash argument is a flag too: reading it as a file path
			// would report the mistake as a confusing open error instead.
			return usageErrorf("unknown flag %q", arg)
		default:
			paths = append(paths, arg)
		}
	}
	if len(paths) == 0 {
		return usageErrorf("dump requires at least one config file")
	}
	for path := range forced {
		if !slices.Contains(paths, path) {
			return usageErrorf("--patch=%s does not name any config file", path)
		}
	}

	layers := make([]loader.Layer, 0, len(paths))
	for index, path := range paths {
		label := path
		patch := index > 0 || strings.HasSuffix(path, ".patch.json") || forced[path]
		layer, err := loadLayer(label, path, patch)
		if err != nil {
			return err
		}
		layers = append(layers, layer)
	}

	tree, err := loader.Compose(layers, options...)
	if err != nil {
		return err
	}
	return tree.Dump(w)
}

func loadLayer(label, path string, patch bool) (loader.Layer, error) {
	if patch {
		return loader.LoadPatchLayer(label, path)
	}
	return loader.LoadLayer(label, path)
}
