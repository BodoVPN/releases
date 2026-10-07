//go:build windows || (linux && !android)

package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"
)

// Prefixes of the lines bodocore writes for its parent; Xray's own log shares stdout.
const (
	statsPrefix = "bodocore-stats "
	pingPrefix  = "bodocore-ping "
)

const usage = `Usage: bodocore run -dns <IP:port> -interface <name> -config <xray.json> [-error-file <path>] [-stats-interval <duration>]
       bodocore ping -dns <IP:port> -interface <name> [-error-file <path>] < ping-request.json`

type options struct {
	command       string
	dns           string
	interfaceName string
	configPath    string
	errorFile     string
	statsInterval time.Duration
}

// commands are run and ping, swappable for tests.
type commands struct {
	run  func(options, io.Writer) error
	ping func(options, io.Reader, io.Writer) error
}

func parseOptions(args []string) (options, error) {
	var opts options
	if len(args) == 0 || (args[0] != "run" && args[0] != "ping") {
		return opts, errors.New("expected the run or ping command")
	}
	opts.command = args[0]
	flags := flag.NewFlagSet(opts.command, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&opts.dns, "dns", "", "DNS server IP endpoint")
	flags.StringVar(&opts.interfaceName, "interface", "", "the uplink the core's own lookups leave by")
	flags.StringVar(&opts.errorFile, "error-file", "", "also write command errors to this file")
	if opts.command == "run" {
		flags.StringVar(&opts.configPath, "config", "", "Xray JSON configuration path")
		flags.DurationVar(&opts.statsInterval, "stats-interval", 0, "write the traffic counters this often")
	}
	if err := flags.Parse(args[1:]); err != nil {
		return opts, err
	}
	if flags.NArg() != 0 {
		return opts, errors.New("unexpected positional arguments")
	}
	if opts.dns == "" || opts.interfaceName == "" {
		return opts, errors.New("dns and interface are required")
	}
	if opts.command == "run" && opts.configPath == "" {
		return opts, errors.New("config is required")
	}
	if opts.statsInterval < 0 {
		return opts, errors.New("stats-interval must not be negative")
	}
	return opts, nil
}

func execute(args []string, cmds commands, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help") {
		fmt.Fprintln(stdout, usage)
		return 0
	}
	opts, err := parseOptions(args)
	if errors.Is(err, flag.ErrHelp) {
		fmt.Fprintln(stdout, usage)
		return 0
	}
	if err == nil && opts.errorFile != "" {
		// Cleared before the run; a caller-made file keeps the caller's permissions.
		if err := os.WriteFile(opts.errorFile, nil, 0o600); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
	}
	if err == nil {
		if opts.command == "run" {
			err = cmds.run(opts, stdout)
		} else {
			err = cmds.ping(opts, stdin, stdout)
		}
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		if opts.errorFile != "" {
			if writeErr := os.WriteFile(opts.errorFile, []byte(err.Error()), 0o600); writeErr != nil {
				fmt.Fprintln(stderr, writeErr)
			}
		}
		return 1
	}
	return 0
}
