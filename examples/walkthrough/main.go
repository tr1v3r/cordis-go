// Command walkthrough prints the effect tree teardown order for a tiny plugin.
package main

import (
	"os"

	cordis "github.com/tr1v3r/cordis-go"
)

func main() {
	rootCtx := cordis.New(cordis.WithWriter(os.Stdout), cordis.WithLevel(cordis.LevelDebug))

	plugin := cordis.Define[struct{}]("demo", func(ctx *cordis.Context, _ struct{}) error {
		ctx.Effect("conn", func() cordis.Disposer {
			ctx.Logger().Info("conn open")
			ctx.OnDispose(func() { ctx.Logger().Info("conn close") }) // nested effect
			return func() { ctx.Logger().Info("conn effect down") }
		})
		ctx.OnDispose(func() { ctx.Logger().Info("second") })
		return nil
	})

	fiber, err := rootCtx.Load(plugin, struct{}{})
	if err != nil {
		panic(err)
	}
	pluginCtx := fiber.Ctx
	for _, meta := range pluginCtx.Effects() {
		pluginCtx.Logger().Info("effect %q children=%d", meta.Label, len(meta.Children()))
	}
	pluginCtx.Logger().Info("--- dispose ---")
	fiber.Dispose()
}
