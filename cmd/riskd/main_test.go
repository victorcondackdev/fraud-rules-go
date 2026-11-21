package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestServeReloadShutdown(t *testing.T) {
	dir := t.TempDir()
	rs := filepath.Join(dir, "rs.json")
	write := func(version, expr string) {
		os.WriteFile(rs, []byte(`{"name":"t","version":"`+version+`","rules":[{"id":"r","expr":"`+expr+`","action":"REVIEW"}]}`), 0o600)
	}
	write("1", "tx.amount > 0")
	ctx, cancel := context.WithCancel(context.Background())
	reload := make(chan os.Signal, 1)
	ready := make(chan string, 1)
	var logs syncBuffer
	done := make(chan int, 1)
	go func() {
		done <- run(ctx, []string{"-addr", "127.0.0.1:0", "-ruleset", rs, "-sweep", "10ms", "-log-level", "debug"}, &logs, reload, ready)
	}()
	addr := <-ready
	get := func(path string) string {
		resp, err := http.Get("http://" + addr + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}
	if body := get("/healthz"); !strings.Contains(body, "ok") {
		t.Errorf("healthz = %s", body)
	}
	write("2", "tx.amount > 1")
	reload <- syscall.SIGHUP
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(get("/v1/rulesets"), `"version": "2"`) {
		if time.Now().After(deadline) {
			t.Fatal("reload not applied")
		}
		time.Sleep(10 * time.Millisecond)
	}
	write("3", "tx.amount >")
	reload <- syscall.SIGHUP
	for !strings.Contains(logs.String(), "reload rejected") {
		if time.Now().After(deadline) {
			t.Fatal("invalid reload not logged")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(get("/v1/rulesets/active"), `"version": "2"`) {
		t.Error("invalid reload replaced the active ruleset")
	}
	time.Sleep(30 * time.Millisecond) // let a sweep tick run
	cancel()
	if code := <-done; code != 0 {
		t.Errorf("exit %d\n%s", code, logs.String())
	}
	if !strings.Contains(logs.String(), `"msg":"riskd stopped"`) {
		t.Errorf("logs: %s", logs.String())
	}
}

func TestStartupErrors(t *testing.T) {
	var errb syncBuffer
	ctx := context.Background()
	for _, args := range [][]string{
		{"-bogus"},
		{"-log-level", "loud"},
	} {
		if code := run(ctx, args, &errb, nil, nil); code != 2 {
			t.Errorf("%v: exit %d", args, code)
		}
	}
	if code := run(ctx, []string{"-ruleset", "/nonexistent.json"}, &errb, nil, nil); code != 1 {
		t.Errorf("missing ruleset: exit %d", code)
	}
	if code := run(ctx, []string{"-ruleset", "../../rulesets/pix-default.json", "-addr", "256.0.0.1:99999"}, &errb, nil, nil); code != 1 {
		t.Errorf("bad addr: exit %d", code)
	}
}
