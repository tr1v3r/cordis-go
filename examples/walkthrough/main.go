// Command walkthrough prints the effect tree teardown order for a tiny plugin.
package main

import (
	"os"

	cordis "github.com/tr1v3r/cordis-go"
)

func main() {
	app := cordis.New(cordis.WithWriter(os.Stdout), cordis.WithLevel(cordis.LevelDebug))

	plugin := cordis.Define[struct{}]("demo", func(ctx *cordis.Context, _ struct{}) error {
		ctx.Effect("conn", func() cordis.Disposer {
			ctx.Logger().Info("conn open")
			ctx.OnDispose(func() { ctx.Logger().Info("conn close") }) // nested effect
			return func() { ctx.Logger().Info("conn effect down") }
		})
		ctx.OnDispose(func() { ctx.Logger().Info("second") })
		return nil
	})

	fiber, err := cordis.Load(app, plugin, struct{}{})
	if err != nil {
		panic(err)
	}
	ctx := fiber.Ctx
	for _, meta := range ctx.Effects() {
		ctx.Logger().Info("effect %q children=%d", meta.Label, len(meta.Children()))
	}
	ctx.Logger().Info("--- dispose ---")
	fiber.Dispose()
}
