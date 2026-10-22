package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/roostlabs/runner/internal/setup"
)

// initConfig runs the setup wizard.
//
// It asks, checks every credential against its service, writes the config at
// 0600 and restarts the Runner when there is a service to restart. Nothing is
// written until the answers make a config the Runner would accept.
func initConfig(args []string) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	configPath := fs.String("config", setup.ConfigPathFor(), "path to the runner config file")
	section := fs.String("section", "", "ask only one section: "+strings.Join(setup.Sections, ", "))
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: %s init [-config path] [-section name]\n\n"+
			"Asks what the Runner needs, checks each credential against its service,\n"+
			"and writes the config file.\n\nOptions:\n", os.Args[0])
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	w := &setup.Wizard{
		In:         os.Stdin,
		Out:        os.Stdout,
		ConfigPath: *configPath,
		Section:    *section,
		Version:    Version,
		Probes:     setup.DefaultProbes(),
		HideEcho:   setup.HideEcho,
	}
	return w.Run(ctx)
}
