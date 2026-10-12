// Package mcp exposes Krunlet's one-shot Runner through a stdio MCP endpoint.
// Only guest-side request values are accepted over the protocol. All host
// configuration is supplied by the trusted process launcher.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/Asutorufa/krunlet"
)

const (
	protocolVersion = "2025-11-25"
	defaultMaxMessageBytes = 16 << 20
)

type Executor interface {
	Run(context.Context, krunlet.Request) (krunlet.Result, error)
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type runArguments struct {
	Command   []string          `json:"command"`
	WorkDir   string            `json:"workdir"`
	Env       map[string]string `json:"env"`
	Stdin     string            `json:"stdin"`
	Files     map[string]string `json:"files"` // base64-encoded guest files
	Collect   []string          `json:"collect"`
	TimeoutMS int64             `json:"timeout_ms"`
}

type server struct {
	executor    Executor
	maxTimeout  time.Duration
	maxFileSize int64
	version     string

	writeMu sync.Mutex
	encoder *json.Encoder

	mu          sync.Mutex
	initialized bool
	active      map[string]context.CancelFunc
	wg          sync.WaitGroup
}

// Serve runs until the input stream closes. It never writes diagnostics or
// unframed data to output. Closing stdin or cancelling ctx cancels all active
// tool calls and waits for the Runner to clean up its helpers.
func Serve(ctx context.Context, input io.Reader, output io.Writer, executor Executor, maxTimeout time.Duration, maxFileSize int64, version string) error {
	return ServeWithMessageLimit(ctx, input, output, executor, maxTimeout, maxFileSize, defaultMaxMessageBytes, version)
}

// ServeWithMessageLimit supports larger base64 inputs without allowing
// unbounded memory use. Oversized lines are fully discarded through the
// next newline, then reported as JSON-RPC errors without closing the session.
func ServeWithMessageLimit(ctx context.Context, input io.Reader, output io.Writer, executor Executor, maxTimeout time.Duration, maxFileSize int64, messageLimit int, version string) error {
	if executor == nil || maxTimeout <= 0 || maxFileSize <= 0 || messageLimit < 1024 || messageLimit > 128<<20 {
		return errors.New("invalid MCP server configuration")
	}
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	s := &server{
		executor: executor, maxTimeout: maxTimeout, maxFileSize: maxFileSize,
		version: version, encoder: json.NewEncoder(output),
		active: make(map[string]context.CancelFunc),
	}
	reader := bufio.NewReaderSize(input, 64<<10)
	var transportErr error
	for {
		line, oversized, err := readFrame(reader, messageLimit)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			transportErr = fmt.Errorf("read MCP message: %w", err)
			break
		}
		if ctx.Err() != nil {
			break
		}
		if oversized {
			transportErr = s.sendError(nil, -32600, "MCP request exceeds the configured frame size limit")
		} else {
			transportErr = s.handle(ctx, line)
		}
		if transportErr != nil {
			break
		}
	}
	wasCancelled := ctx.Err() != nil
	stop()
	s.cancelAll()
	s.wg.Wait()
	if transportErr != nil && !wasCancelled {
		return transportErr
	}
	return nil
}

// readFrame bounds allocations even if a peer sends an unlimited line.
// When a line is too long, keep consuming chunks until a newline so the
// next read starts on an intact JSON-RPC frame. EOF after a partial final
// frame is handled once, then the caller sees EOF on the next read.
func readFrame(reader *bufio.Reader, limit int) ([]byte, bool, error) {
	frame := make([]byte, 0, min(limit, 64<<10))
	tooLarge := false
	for {
		chunk, err := reader.ReadSlice('\n')
		if !tooLarge {
			if len(chunk) > limit-len(frame) {
				tooLarge = true
			} else {
				frame = append(frame, chunk...)
			}
		}
		switch {
		case err == nil:
			return frame, tooLarge, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			if len(frame) == 0 && !tooLarge {
				return nil, false, io.EOF
			}
			return frame, tooLarge, nil
		default:
			return nil, false, err
		}
	}
}

func (s *server) handle(ctx context.Context, line []byte) error {
	var req request
	if err := json.Unmarshal(line, &req); err != nil {
		return s.sendError(nil, -32700, "Parse error")
	}
	if req.JSONRPC != "2.0" || req.Method == "" || !validID(req.ID) {
		return s.sendError(nil, -32600, "Invalid Request")
	}
	// A missing ID denotes a notification. Never execute a guest command
	// without a request ID, since the caller could not cancel it.
	if len(req.ID) == 0 {
		if req.Method == "notifications/cancelled" {
			var params struct {
				RequestID json.RawMessage `json:"requestId"`
			}
			if json.Unmarshal(req.Params, &params) == nil && validID(params.RequestID) && len(params.RequestID) != 0 {
				s.mu.Lock()
				cancel := s.active[string(params.RequestID)]
				s.mu.Unlock()
				if cancel != nil {
					cancel()
				}
			}
		}
		return nil
	}
	switch req.Method {
	case "initialize":
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if json.Unmarshal(req.Params, &params) != nil || params.ProtocolVersion == "" {
			return s.sendError(req.ID, -32602, "protocolVersion is required")
		}
		version := protocolVersion
		switch params.ProtocolVersion {
		case "2024-11-05", "2025-03-26", "2025-06-18", protocolVersion:
			version = params.ProtocolVersion
		}
		s.mu.Lock()
		s.initialized = true
		s.mu.Unlock()
		return s.send(req.ID, map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]string{"name": "krunlet", "version": s.version},
			"instructions":    "Commands run inside disposable microVMs. Host configuration and network access are fixed by the trusted server operator.",
		})
	case "ping":
		return s.send(req.ID, map[string]any{})
	case "tools/list":
		if !s.isInitialized() {
			return s.sendError(req.ID, -32000, "Server is not initialized")
		}
		return s.send(req.ID, map[string]any{"tools": []any{runTool()}})
	case "tools/call":
		if !s.isInitialized() {
			return s.sendError(req.ID, -32000, "Server is not initialized")
		}
		var params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if json.Unmarshal(req.Params, &params) != nil || params.Name != "krunlet_run" {
			return s.sendError(req.ID, -32602, "Unknown or invalid tool")
		}
		key := string(req.ID)
		callCtx, cancel := context.WithCancel(ctx)
		s.mu.Lock()
		if _, exists := s.active[key]; exists {
			s.mu.Unlock()
			cancel()
			return s.sendError(req.ID, -32600, "Duplicate request ID")
		}
		s.active[key] = cancel
		s.wg.Add(1)
		s.mu.Unlock()
		go func() {
			defer s.wg.Done()
			defer func() {
				s.mu.Lock()
				delete(s.active, key)
				s.mu.Unlock()
				cancel()
			}()
			result := s.call(callCtx, params.Arguments)
			// A cancelled request no longer has a consumer. Do not emit
			// a stale result after notifications/cancelled or stdin EOF.
			if callCtx.Err() == nil {
				_ = s.send(req.ID, result)
			}
		}()
		return nil
	default:
		return s.sendError(req.ID, -32601, "Method not found")
	}
}

func validID(id json.RawMessage) bool {
	if len(id) == 0 {
		return true
	}
	if id[0] == '"' {
		var value string
		return json.Unmarshal(id, &value) == nil
	}
	if id[0] == '-' || (id[0] >= '0' && id[0] <= '9') {
		var value json.Number
		return json.Unmarshal(id, &value) == nil
	}
	return false
}

func (s *server) isInitialized() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.initialized
}

func (s *server) cancelAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, cancel := range s.active {
		cancel()
	}
}

func (s *server) send(id json.RawMessage, result any) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.encoder.Encode(response{JSONRPC: "2.0", ID: id, Result: result})
}

func (s *server) sendError(id json.RawMessage, code int, message string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.encoder.Encode(response{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: message}})
}

func (s *server) call(ctx context.Context, raw json.RawMessage) any {
	var args runArguments
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&args); err != nil {
		return toolError("invalid arguments: " + err.Error())
	}
	if len(args.Command) == 0 || len(args.Command) > 256 {
		return toolError("command must contain 1 to 256 arguments")
	}
	if args.TimeoutMS < 0 || args.TimeoutMS > s.maxTimeout.Milliseconds() {
		return toolError(fmt.Sprintf("timeout_ms must be between 0 and %d", s.maxTimeout.Milliseconds()))
	}
	if len(args.Files) > 16 || len(args.Collect) > 16 || len(args.Env) > 128 {
		return toolError("too many files, collected paths, or environment variables")
	}
	files := make(map[string][]byte, len(args.Files))
	for name, encoded := range args.Files {
		if int64(base64.StdEncoding.DecodedLen(len(encoded))) > s.maxFileSize+2 {
			return toolError(fmt.Sprintf("input %q exceeds the file limit", name))
		}
		content, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return toolError(fmt.Sprintf("input %q is not valid base64", name))
		}
		if int64(len(content)) > s.maxFileSize {
			return toolError(fmt.Sprintf("input %q exceeds the file limit", name))
		}
		files[name] = content
	}
	req := krunlet.Request{
		Command: args.Command, WorkDir: args.WorkDir, Env: args.Env,
		Stdin: args.Stdin, Files: files, Collect: args.Collect,
	}
	if args.TimeoutMS != 0 {
		req.Timeout = time.Duration(args.TimeoutMS) * time.Millisecond
	}
	result, err := s.executor.Run(ctx, req)
	out := map[string]any{
		"exit_code":      result.ExitCode,
		"run_id":         result.RunID,
		"stdout":         result.Stdout,
		"stderr":         result.Stderr,
		"duration_ms":    result.Duration.Milliseconds(),
		"timed_out":      result.TimedOut,
		"output_limited": result.OutputLimited,
		"containment":    result.Containment,
	}
	if result.TerminationReason != "" {
		out["termination_reason"] = result.TerminationReason
	}
	if result.MemoryPeakBytes != nil {
		out["memory_peak_bytes"] = *result.MemoryPeakBytes
	}
	if len(result.Files) > 0 {
		out["files"] = result.Files // JSON's []byte encoding is base64.
	}
	if err != nil {
		out["error"] = err.Error()
	}
	data, marshalErr := json.Marshal(out)
	if marshalErr != nil {
		return toolError("encode tool result: " + marshalErr.Error())
	}
	return map[string]any{
		"content":           []any{map[string]string{"type": "text", "text": string(data)}},
		"structuredContent": out,
		"isError":           err != nil || result.ExitCode != 0,
	}
}

func toolError(message string) any {
	return map[string]any{
		"content": []any{map[string]string{"type": "text", "text": message}},
		"isError": true,
	}
}

func runTool() any {
	return map[string]any{
		"name":        "krunlet_run",
		"description": "Run one command in a new disposable Krunlet microVM; no host paths or VM settings can be changed by the tool caller. Nonzero guest exits are reported in the result.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "minItems": 1, "description": "Guest executable and arguments, e.g. [\"/bin/sh\",\"-lc\",\"echo hi\"]"},
				"workdir":    map[string]any{"type": "string", "description": "Absolute guest working directory (default /)"},
				"stdin":      map[string]any{"type": "string", "description": "Literal stdin for the guest command"},
				"env":        map[string]any{"type": "object", "additionalProperties": map[string]string{"type": "string"}, "description": "Guest environment variables"},
				"files":      map[string]any{"type": "object", "additionalProperties": map[string]string{"type": "string"}, "description": "Absolute guest paths mapped to base64 file contents. Does not read host files."},
				"collect":    map[string]any{"type": "array", "items": map[string]string{"type": "string"}, "description": "Absolute guest paths to collect after command exits (returned as base64)."},
				"timeout_ms": map[string]any{"type": "integer", "minimum": 1, "description": "Optional shorter timeout; cannot exceed the server's configured maximum."},
			},
			"required":             []string{"command"},
			"additionalProperties": false,
		},
	}
}
