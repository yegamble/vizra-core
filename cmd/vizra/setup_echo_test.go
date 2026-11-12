package main

import (
	"errors"
	"os"
	"syscall"
	"testing"
)

// A signal while echo is off must restore echo before exiting.
func TestWithEchoOffRestoresOnSignal(t *testing.T) {
	sigs := make(chan os.Signal, 1)
	restored, code := 0, -1
	block := make(chan struct{})
	done := make(chan struct{})
	go func() {
		_, _ = withEchoOff(func() error { return nil }, func() error { restored++; return nil }, sigs,
			func(c int) { code = c; close(done) }, func() (string, error) { <-block; return "", errors.New("x") })
	}()
	sigs <- syscall.SIGINT
	<-done
	if restored != 1 || code != 130 {
		t.Fatalf("restored=%d code=%d, want 1/130", restored, code)
	}
	close(block)
}

func TestWithEchoOffRestoresOnReturn(t *testing.T) {
	restored := 0
	v, err := withEchoOff(func() error { return nil }, func() error { restored++; return nil }, make(chan os.Signal),
		func(int) { t.Fatal("exit called") }, func() (string, error) { return "s3cret", nil })
	if err != nil || v != "s3cret" || restored != 1 {
		t.Fatalf("v=%q err=%v restored=%d", v, err, restored)
	}
	if _, err := withEchoOff(func() error { return errors.New("no tty") }, func() error { restored++; return nil }, nil, nil, nil); err == nil || restored != 1 {
		t.Fatal("disable failure must not call restore or read")
	}
}
