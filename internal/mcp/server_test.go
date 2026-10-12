package mcp

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Asutorufa/krunlet"
)

type fakeExecutor func(context.Context, krunlet.Request) (krunlet.Result, error)

func (f fakeExecutor) Run(ctx context.Context, req krunlet.Request) (krunlet.Result, error) {
	return f(ctx, req)
}

type testTransport struct {
	input *io.PipeWriter
	out   *bufio.Reader
	done  chan error
}

func newTransport(t *testing.T, runner Executor) *testTransport {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- Serve(context.Background(), inR, outW, runner, 3*time.Second, 4096, "test")
		_ = outW.Close()
	}()
	t.Cleanup(func() {
		_ = inW.Close()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Serve: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("Serve did not exit on EOF")
		}
		_ = outR.Close()
	})
	return &testTransport{input: inW, out: bufio.NewReader(outR), done: done}
}

func (tr *testTransport) send(t *testing.T, message string) {
	t.Helper()
	if _, err := io.WriteString(tr.input, message+"\n"); err != nil {
		t.Fatal(err)
	}
}

func (tr *testTransport) read(t *testing.T) map[string]any {
	t.Helper()
	line, err := tr.out.ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var response map[string]any
	if err := json.Unmarshal(line, &response); err != nil {
		t.Fatalf("invalid JSON-RPC output %s: %v", line, err)
	}
	if response["jsonrpc"] != "2.0" {
		t.Fatalf("not a JSON-RPC message: %v", response)
	}
	return response
}

func initialize(t *testing.T, tr *testTransport) {
	t.Helper()
	tr.send(t, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`)
	resp := tr.read(t)
	result := resp["result"].(map[string]any)
	if result["protocolVersion"] != protocolVersion {
		t.Fatalf("protocol negotiation: %v", result)
	}
}

func TestInitializeListAndRun(t *testing.T) {
	var called atomic.Int32
	tr := newTransport(t, fakeExecutor(func(_ context.Context, req krunlet.Request) (krunlet.Result, error) {
		called.Add(1)
		if len(req.Command) != 3 || req.Command[0] != "/bin/sh" || req.Stdin != "input" || req.Env["LANG"] != "C" || req.Timeout != time.Second {
			t.Errorf("incorrect guest request: %+v", req)
		}
		if string(req.Files["/tmp/in.txt"]) != "hello" || len(req.Collect) != 1 || req.Collect[0] != "/tmp/out.txt" {
			t.Errorf("incorrect guest files: %+v", req)
		}
		return krunlet.Result{ExitCode: 0, Stdout: "hello\n", Files: map[string][]byte{"/tmp/out.txt": []byte("done")}, Containment: "cgroup-v2"}, nil
	}))
	initialize(t, tr)
	tr.send(t, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	tr.send(t, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	tools := tr.read(t)["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["name"] != "krunlet_run" {
		t.Fatalf("bad tools/list: %v", tools)
	}
	encoded := base64.StdEncoding.EncodeToString([]byte("hello"))
	tr.send(t, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"krunlet_run","arguments":{"command":["/bin/sh","-lc","cat"],"stdin":"input","env":{"LANG":"C"},"files":{"/tmp/in.txt":"`+encoded+`"},"collect":["/tmp/out.txt"],"timeout_ms":1000}}}`)
	response := tr.read(t)
	if response["id"] != float64(3) {
		t.Fatalf("response ID: %v", response)
	}
	result := response["result"].(map[string]any)
	if result["isError"] != false {
		t.Fatalf("tool error: %v", result)
	}
	structured := result["structuredContent"].(map[string]any)
	if structured["stdout"] != "hello\n" || structured["files"].(map[string]any)["/tmp/out.txt"] != base64.StdEncoding.EncodeToString([]byte("done")) {
		t.Fatalf("result content: %v", structured)
	}
	var textContent map[string]any
	if err := json.Unmarshal([]byte(result["content"].([]any)[0].(map[string]any)["text"].(string)), &textContent); err != nil || textContent["stdout"] != structured["stdout"] {
		t.Fatalf("text fallback isn't JSON: %v, %v", textContent, err)
	}
	if called.Load() != 1 {
		t.Fatalf("got %d calls, want 1", called.Load())
	}
}

func TestProtocolErrorsAndInvalidToolInputs(t *testing.T) {
	var called atomic.Int32
	tr := newTransport(t, fakeExecutor(func(_ context.Context, _ krunlet.Request) (krunlet.Result, error) {
		called.Add(1)
		return krunlet.Result{}, nil
	}))
	tr.send(t, "{")
	if got := tr.read(t)["error"].(map[string]any)["code"]; got != float64(-32700) {
		t.Fatalf("parse error code: %v", got)
	}
	tr.send(t, `{"jsonrpc":"2.0","id":7,"method":"tools/list"}`)
	if got := tr.read(t)["error"].(map[string]any)["code"]; got != float64(-32000) {
		t.Fatalf("missing initialization error: %v", got)
	}
	initialize(t, tr)
	tr.send(t, `{"jsonrpc":"2.0","id":8,"method":"resources/list"}`)
	if got := tr.read(t)["error"].(map[string]any)["code"]; got != float64(-32601) {
		t.Fatalf("missing method error: %v", got)
	}
	tr.send(t, `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"other","arguments":{}}}`)
	if got := tr.read(t)["error"].(map[string]any)["code"]; got != float64(-32602) {
		t.Fatalf("unknown tool error: %v", got)
	}
	for i, arguments := range []string{
		`{"command":[]}`,
		`{"command":["echo"],"files":{"/tmp/data":"%%%bad"}}`,
		`{"command":["echo"],"timeout_ms":4000}`,
		`{"command":["echo"],"host_path":"/etc/passwd"}`,
	} {
		tr.send(t, `{"jsonrpc":"2.0","id":`+strconv.Itoa(10+i)+`,"method":"tools/call","params":{"name":"krunlet_run","arguments":`+arguments+`}}`)
		if got := tr.read(t)["result"].(map[string]any)["isError"]; got != true {
			t.Fatalf("expected validation error for %s, got %v", arguments, got)
		}
	}
	if called.Load() != 0 {
		t.Fatalf("unexpected guest executions: %d", called.Load())
	}
}

func TestToolExecutionFailureIsToolResult(t *testing.T) {
	tr := newTransport(t, fakeExecutor(func(context.Context, krunlet.Request) (krunlet.Result, error) {
		return krunlet.Result{ExitCode: -1, TimedOut: true}, errors.New("deadline exceeded")
	}))
	initialize(t, tr)
	tr.send(t, `{"jsonrpc":"2.0","id":"job","method":"tools/call","params":{"name":"krunlet_run","arguments":{"command":["/bin/sleep","60"]}}}`)
	result := tr.read(t)["result"].(map[string]any)
	if result["isError"] != true || result["structuredContent"].(map[string]any)["timed_out"] != true {
		t.Fatalf("execution failure not propagated: %v", result)
	}
}

func TestCancellationSuppressesReplyAndStopsRunner(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan struct{})
	tr := newTransport(t, fakeExecutor(func(ctx context.Context, _ krunlet.Request) (krunlet.Result, error) {
		close(started)
		<-ctx.Done()
		close(cancelled)
		return krunlet.Result{}, ctx.Err()
	}))
	initialize(t, tr)
	tr.send(t, `{"jsonrpc":"2.0","id":42,"method":"tools/call","params":{"name":"krunlet_run","arguments":{"command":["/bin/sleep","60"]}}}`)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("runner never started")
	}
	tr.send(t, `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":42}}`)
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation did not reach runner")
	}
	tr.send(t, `{"jsonrpc":"2.0","id":43,"method":"ping"}`)
	if response := tr.read(t); response["id"] != float64(43) {
		t.Fatalf("cancelled tool sent a response: %v", response)
	}
}

func TestEOFReapsActiveCalls(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan struct{})
	inR, inW := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- Serve(context.Background(), inR, io.Discard, fakeExecutor(func(ctx context.Context, _ krunlet.Request) (krunlet.Result, error) {
			close(started)
			<-ctx.Done()
			close(cancelled)
			return krunlet.Result{}, ctx.Err()
		}), time.Second, 4096, "test")
	}()
	_, _ = io.WriteString(inW, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`+"\n")
	_, _ = io.WriteString(inW, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"krunlet_run","arguments":{"command":["sleep","5"]}}}`+"\n")
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("guest never started")
	}
	_ = inW.Close()
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("EOF did not cancel guest")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve after EOF: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not wait for cancelled guest")
	}
}

func TestOversizedInputIsRejected(t *testing.T) {
	err := Serve(context.Background(), strings.NewReader(strings.Repeat(" ", maxMessageBytes+1)+"\n"), io.Discard,
		fakeExecutor(func(context.Context, krunlet.Request) (krunlet.Result, error) {
			t.Fatal("runner called")
			return krunlet.Result{}, nil
		}), time.Second, 4096, "test")
	if err == nil {
		t.Fatal("oversized MCP frame accepted")
	}
}

func TestInputFileAtExactLimit(t *testing.T) {
	tr := newTransport(t, fakeExecutor(func(_ context.Context, req krunlet.Request) (krunlet.Result, error) {
		if got := len(req.Files["/tmp/at-limit"]); got != 4096 {
			t.Errorf("decoded guest file size %d, want 4096", got)
		}
		return krunlet.Result{ExitCode: 0}, nil
	}))
	initialize(t, tr)
	data := base64.StdEncoding.EncodeToString(make([]byte, 4096))
	tr.send(t, `{"jsonrpc":"2.0","id":55,"method":"tools/call","params":{"name":"krunlet_run","arguments":{"command":["true"],"files":{"/tmp/at-limit":"`+data+`"}}}}`)
	result := tr.read(t)["result"].(map[string]any)
	if result["isError"] != false {
		t.Fatalf("exact-size file was rejected: %v", result)
	}
}

func TestContextCancellationWithClosedInput(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	input, writer := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, input, io.Discard, fakeExecutor(func(context.Context, krunlet.Request) (krunlet.Result, error) {
			t.Error("runner called")
			return krunlet.Result{}, nil
		}), time.Second, 4096, "test")
	}()
	cancel()
	_ = writer.CloseWithError(io.ErrClosedPipe)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("cancelled MCP shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled MCP server did not exit")
	}
}
