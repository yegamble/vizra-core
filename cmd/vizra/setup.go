package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/yegamble/vizra-core/internal/setup"
)

type setFlags map[string]string

func (s setFlags) String() string { return "" }
func (s setFlags) Set(v string) error {
	k, val, ok := strings.Cut(v, "=")
	if !ok {
		return errors.New("--set wants id=value")
	}
	s[k] = val
	return nil
}

// runSetup is `vizra setup` (VZ-INSTALL-002). It runs at the install root and
// writes env/production.env there. --web is not built yet and is refused.
func runSetup(args []string) error {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	envPath := fs.String("env", "env/production.env", "env file to write or check")
	nonInteractive := fs.Bool("non-interactive", false, "never prompt; use --answers/--set/VIZRA_SETUP_<ID> and defaults")
	answersPath := fs.String("answers", "", "answers file: id=value lines (values may be @file or -)")
	check := fs.Bool("check", false, "validate the existing env file; change nothing")
	rotate := fs.String("rotate", "", "rotate one generated secret by question id (needs --yes)")
	yes := fs.Bool("yes", false, "allow rewriting an existing env file")
	yesIKnow := fs.Bool("yes-i-know", false, "also required to rotate mfa_kek")
	web := fs.Bool("web", false, "loopback web wizard (not built yet)")
	sets := setFlags{}
	fs.Var(sets, "set", "id=value answer (repeatable)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	opts := setup.Options{Path: *envPath, Out: os.Stdout, Stdin: os.Stdin, Yes: *yes, Answers: map[string]string{}}
	switch {
	case *web:
		return errors.New("vizra setup --web: the web wizard is not built yet (VZ-INSTALL-002 continuation); use the terminal interview")
	case *check:
		env, err := setup.ReadEnvFile(*envPath)
		if err != nil {
			return err
		}
		if errs := setup.Check(env); len(errs) > 0 {
			return fmt.Errorf("vizra setup --check: %d problem(s):\n  %s", len(errs), strings.Join(errs, "\n  "))
		}
		fmt.Printf("%s: OK\n", *envPath)
		return nil
	case *rotate != "":
		env, err := setup.ReadEnvFile(*envPath)
		if err != nil {
			return err
		}
		n, err := setup.Rotate(env, *rotate, *yes, *yesIKnow)
		if err != nil {
			return err
		}
		return setup.WriteEnv(opts, n)
	}
	if *answersPath != "" {
		a, err := readAnswers(*answersPath)
		if err != nil {
			return err
		}
		opts.Answers = a
	}
	for k, v := range sets {
		opts.Answers[k] = v
	}
	if !*nonInteractive {
		if !isTTY() {
			return errors.New("vizra setup: stdin is not a terminal; use --non-interactive")
		}
		opts.Ask = &setup.LineAsker{In: bufio.NewReader(os.Stdin), Out: os.Stdout, ReadSecret: readNoEcho}
	}
	return setup.Run(opts)
}

func readAnswers(path string) (map[string]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	a := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("%s: malformed line (no '=')", path)
		}
		a[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return a, nil
}

func isTTY() bool {
	st, err := os.Stdin.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

// readNoEcho reads one line with terminal echo off via stty (no new module
// dependency: golang.org/x/term is not in go.mod).
func readNoEcho() (string, error) {
	off := exec.Command("stty", "-echo")
	off.Stdin = os.Stdin
	if err := off.Run(); err != nil {
		return "", fmt.Errorf("cannot disable terminal echo: %w", err)
	}
	defer func() {
		on := exec.Command("stty", "echo")
		on.Stdin = os.Stdin
		_ = on.Run()
	}()
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}
