//go:build windows || (linux && !android)

package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func noCommands(t *testing.T) commands {
	return commands{
		run:  func(options, io.Writer) error { t.Fatal("run called"); return nil },
		ping: func(options, io.Reader, io.Writer) error { t.Fatal("ping called"); return nil },
	}
}

func TestRunKeepsLibXraysFlagsAndAddsTheStatsInterval(t *testing.T) {
	opts, err := parseOptions([]string{"run", "-dns", "8.8.8.8:53", "-interface", "Ethernet 2",
		"-config", `C:\run\xray.json`, "-error-file", "e.txt", "-stats-interval", "1s"})
	if err != nil {
		t.Fatal(err)
	}
	if opts.dns != "8.8.8.8:53" || opts.interfaceName != "Ethernet 2" || opts.configPath != `C:\run\xray.json` ||
		opts.errorFile != "e.txt" || opts.statsInterval != time.Second {
		t.Fatalf("options = %+v", opts)
	}
	for name, args := range map[string][]string{
		"no command":        {},
		"unknown command":   {"version"},
		"no dns":            {"run", "-interface", "eth0", "-config", "x"},
		"no interface":      {"ping", "-dns", "8.8.8.8:53"},
		"no config":         {"run", "-dns", "8.8.8.8:53", "-interface", "eth0"},
		"config on ping":    {"ping", "-dns", "8.8.8.8:53", "-interface", "eth0", "-config", "x"},
		"negative interval": {"run", "-dns", "8.8.8.8:53", "-interface", "eth0", "-config", "x", "-stats-interval", "-1s"},
		"positional":        {"run", "-dns", "8.8.8.8:53", "-interface", "eth0", "-config", "x", "extra"},
	} {
		if _, err := parseOptions(args); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestHelpPrintsBothCommands(t *testing.T) {
	var stdout bytes.Buffer
	if code := execute([]string{"-h"}, noCommands(t), nil, &stdout, io.Discard); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.HasPrefix(stdout.String(), "Usage: bodocore run") || !strings.Contains(stdout.String(), "bodocore ping") {
		t.Fatalf("usage = %q", stdout.String())
	}
	if code := execute(nil, noCommands(t), nil, io.Discard, io.Discard); code == 0 {
		t.Fatal("no arguments exited 0")
	}
}

func TestAFailureLandsOnStderrAndInTheErrorFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "core.error")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmds := noCommands(t)
	cmds.run = func(options, io.Writer) error { return errors.New("failed to load geosite") }
	code := execute([]string{"run", "-dns", "1.1.1.1:53", "-interface", "eth0", "-config", "x", "-error-file", path},
		cmds, nil, io.Discard, &stderr)
	data, _ := os.ReadFile(path)
	if code != 1 || string(data) != "failed to load geosite" || stderr.String() != "failed to load geosite\n" {
		t.Fatalf("code %d, file %q, stderr %q", code, data, stderr.String())
	}
}

func TestPingHandsStdinToThePingCommand(t *testing.T) {
	cmds := noCommands(t)
	got := ""
	cmds.ping = func(opts options, stdin io.Reader, stdout io.Writer) error {
		raw, _ := io.ReadAll(stdin)
		got = string(raw)
		_, err := io.WriteString(stdout, pingPrefix+`{"index":0}`+"\n")
		return err
	}
	var stdout bytes.Buffer
	code := execute([]string{"ping", "-dns", "1.1.1.1:53", "-interface", "eth0"}, cmds, strings.NewReader("{}"), &stdout, io.Discard)
	if code != 0 || got != "{}" || !strings.HasPrefix(stdout.String(), pingPrefix) {
		t.Fatalf("code %d, stdin %q, stdout %q", code, got, stdout.String())
	}
}

func TestLinesAreWholeAndPrefixed(t *testing.T) {
	var buffer bytes.Buffer
	out := &lines{out: &buffer}
	if err := out.write(statsPrefix, map[string]int{"a": 1}); err != nil {
		t.Fatal(err)
	}
	if buffer.String() != "bodocore-stats {\"a\":1}\n" {
		t.Fatalf("line = %q", buffer.String())
	}
}
