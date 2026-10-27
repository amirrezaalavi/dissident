// Command dissident is the single-process demo build: one config, one binary,
// one control panel, and (in later milestones) one proxy.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"syscall"

	"dissident/internal/app"
	"dissident/internal/config"
)

// version is overridden at build time with -ldflags "-X main.version=...".
var version = "dev"

const (
	exitOK      = 0
	exitRuntime = 1
	exitUsage   = 2
	exitConfig  = 3
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("dissident", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "./configs/dissident.conf", "path to the INI configuration file")
	checkConfig := fs.Bool("check-config", false, "validate the configuration file and exit")
	showVersion := fs.Bool("version", false, "print the version and exit")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: dissident [-config path] [-check-config] [-version]")
		fmt.Fprintln(stderr)
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "dissident: unexpected arguments: %v\n", fs.Args())
		fs.Usage()
		return exitUsage
	}
	if *showVersion {
		fmt.Fprintf(stdout, "dissident %s\n", version)
		return exitOK
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "dissident: config error: %v\n", err)
		return exitConfig
	}
	if *checkConfig {
		fmt.Fprintf(stdout, "config OK: %s\n", *configPath)
		return exitOK
	}

	application, err := app.New(app.Options{
		Version:    version,
		ConfigPath: *configPath,
		Config:     cfg,
		Stderr:     stderr,
	})
	if err != nil {
		fmt.Fprintf(stderr, "dissident: startup error: %v\n", err)
		return exitRuntime
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// SIGHUP reload is Unix-only: Windows has no HUP delivery to a console
	// process. Windows operators restart the service to apply the same fields.
	if runtime.GOOS != "windows" {
		hup := make(chan os.Signal, 1)
		signal.Notify(hup, syscall.SIGHUP)
		defer signal.Stop(hup)

		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case <-hup:
					if ctx.Err() != nil {
						return
					}
					if err := application.ReloadConfig(); err != nil {
						application.Logger().Error("config reload rejected; keeping previous snapshot", "err", err)
						continue
					}
					application.Logger().Info("config reloaded via SIGHUP")
				}
			}
		}()
	}

	if err := application.Run(ctx); err != nil {
		fmt.Fprintf(stderr, "dissident: %v\n", err)
		return exitRuntime
	}
	return exitOK
}
