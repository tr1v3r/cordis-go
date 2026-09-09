// Command cordis inspects cordis-go configuration layers.
//
// It is the Go equivalent of `dsh --profile <name> --dump-config`: composition
// is pure data, so the dump needs no plugin registry and never runs plugin code.
//
//	cordis dump base.json            # base layer only
//	cordis dump base.json profile.json
//	cordis dump --strict base.json profile.json
//
// A file whose name ends with .patch.json is treated as a patch layer; every
// other file is treated as a base layer. All but the first layer may also be
// forced to patch mode with --patch=<path>.
package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/tr1v3r/cordis-go/loader"
)

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}
	switch args[0] {
	case "dump":
		if err := dump(args[1:]); err != nil {
			fmt.Fprintln(os.Stderr, "cordis:", err)
			os.Exit(1)
		}
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "cordis: unknown command %q\n", args[0])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage:
  cordis dump [--strict] <file.json>...   compose layers and print the tree
`)
}

func dump(args []string) error {
	options := []loader.ComposeOption{}
	var paths []string
	for _, arg := range args {
		switch {
		case arg == "--strict":
			options = append(options, loader.Strict())
		case strings.HasPrefix(arg, "--"):
			return fmt.Errorf("unknown flag %q", arg)
		default:
			paths = append(paths, arg)
		}
	}
	if len(paths) == 0 {
		return fmt.Errorf("dump requires at least one config file")
	}

	layers := make([]loader.Layer, 0, len(paths))
	for index, path := range paths {
		label := path
		patch := index > 0 || strings.HasSuffix(path, ".patch.json")
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
	return tree.Dump(os.Stdout)
}

func loadLayer(label, path string, patch bool) (loader.Layer, error) {
	if patch {
		return loader.LoadPatchLayer(label, path)
	}
	return loader.LoadLayer(label, path)
}
