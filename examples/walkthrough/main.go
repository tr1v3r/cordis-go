// Command walkthrough prints the effect tree teardown order for a tiny plugin.
//
// It shows the two rules that make an unload reversible:
//
//   - top-level effects unwind newest-first;
//   - an effect registered while another effect's body runs becomes its child,
//     and a child unwinds after its owner.
package main

import (
	"os"

	cordis "github.com/tr1v3r/cordis-go"
)

func main() {
	rootCtx := cordis.New(cordis.WithWriter(os.Stdout), cordis.WithLevel(cordis.LevelDebug))

	plugin := cordis.Define[struct{}]("demo", func(ctx *cordis.Context, _ struct{}) error {
		// "conn" owns everything registered while its body runs, so the whole
		// subtree tears down as one unit.
		ctx.Effect("conn", func() cordis.Disposer {
			ctx.Logger().Info("conn open")
			ctx.OnDispose(func() { ctx.Logger().Info("conn close") }) // child of "conn"
			return func() { ctx.Logger().Info("conn effect down") }
		})

		// Registered after "conn" returned, so it is a sibling, not a child.
		// OnDispose carries no label of its own, so the tree prints it as
		// "anonymous"; use ctx.Effect("name", ...) when the label matters.
		ctx.OnDispose(func() { ctx.Logger().Info("second") })
		return nil
	})

	fiber, err := rootCtx.Load(plugin, struct{}{})
	if err != nil {
		panic(err)
	}
	pluginCtx := fiber.Ctx

	// Effects() lists the live tree in registration order; Dispose unwinds the
	// same list newest-first.
	for _, meta := range pluginCtx.Effects() {
		pluginCtx.Logger().Info("effect %q children=%d", meta.Label, len(meta.Children()))
	}
	pluginCtx.Logger().Info("--- dispose ---")
	fiber.Dispose() // second → conn effect down → conn close
}
