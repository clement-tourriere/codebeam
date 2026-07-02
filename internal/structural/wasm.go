// Package structural implements structural (AST) code search. Pattern
// matching is done by ast-grep-core compiled to a WASI module (see
// wasm/astgrep) and executed in-process on wazero — no external binary, no
// cgo, one platform-neutral artifact embedded in the Codebeam binary.
package structural

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"sync"
	"sync/atomic"

	"github.com/tetratelabs/wazero"
	wazeroapi "github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

//go:embed astgrep.wasm
var astGrepWasm []byte

// Span is a zero-based byte range into the source passed to MatchBytes.
type Span struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

// WasmMatch is one structural match within a single file. Offsets are
// zero-based bytes; lines are zero-based (callers converting to Codebeam's
// UI convention must add 1).
type WasmMatch struct {
	ByteStart int             `json:"byteStart"`
	ByteEnd   int             `json:"byteEnd"`
	StartLine int             `json:"startLine"`
	EndLine   int             `json:"endLine"`
	Vars      map[string]Span `json:"vars"`
	MultiVars map[string]Span `json:"multiVars"`
}

type matchesOut struct {
	Matches   []WasmMatch `json:"matches"`
	Truncated bool        `json:"truncated"`
}

// Matcher runs compiled structural patterns against file contents using a
// pool of single-threaded WASM module instances. It is safe for concurrent
// use; parallelism is bounded by PoolSize.
type Matcher struct {
	// CacheDir, when set, persists wazero's compiled-module cache across
	// restarts so the embedded module does not need recompiling on boot.
	CacheDir string
	// PoolSize bounds concurrent module instances. Defaults to
	// min(GOMAXPROCS, 8).
	PoolSize int

	initOnce sync.Once
	initErr  error
	rt       wazero.Runtime
	compiled wazero.CompiledModule
	idle     chan *instance
	created  atomic.Int32
	nextName atomic.Int64
}

// instance wraps one instantiated module. Instances are not safe for
// concurrent use; the Matcher pool hands them out exclusively.
type instance struct {
	mod        wazeroapi.Module
	fnAlloc    wazeroapi.Function
	fnDealloc  wazeroapi.Function
	fnCompile  wazeroapi.Function
	fnMatch    wazeroapi.Function
	fnFree     wazeroapi.Function
	fnLastErr  wazeroapi.Function
	fnMeta     wazeroapi.Function
	patternIDs map[string]uint32 // lang+"\x00"+pattern → shim handle
}

func (m *Matcher) poolSize() int {
	if m.PoolSize > 0 {
		return m.PoolSize
	}
	n := runtime.GOMAXPROCS(0)
	if n > 8 {
		n = 8
	}
	if n < 1 {
		n = 1
	}
	return n
}

func (m *Matcher) init(ctx context.Context) error {
	m.initOnce.Do(func() {
		// CloseOnContextDone would let a context cancel a wasm call mid-flight,
		// but its instrumentation costs ~3x on tree-sitter's hot loops
		// (benchmarked 31ms → 10ms per file). Parses finish in single-digit
		// milliseconds, so timeouts are enforced between files by the engine
		// instead, and a trapped instance is discarded rather than reused.
		cfg := wazero.NewRuntimeConfig().
			WithCloseOnContextDone(false).
			WithMemoryLimitPages(16384) // 1 GiB per instance, hard cap
		if m.CacheDir != "" {
			if err := os.MkdirAll(m.CacheDir, 0o755); err == nil {
				if cache, err := wazero.NewCompilationCacheWithDir(m.CacheDir); err == nil {
					cfg = cfg.WithCompilationCache(cache)
				}
			}
		}
		m.rt = wazero.NewRuntimeWithConfig(ctx, cfg)
		if _, err := wasi_snapshot_preview1.Instantiate(ctx, m.rt); err != nil {
			m.initErr = fmt.Errorf("structural: instantiate WASI: %w", err)
			return
		}
		compiled, err := m.rt.CompileModule(ctx, astGrepWasm)
		if err != nil {
			m.initErr = fmt.Errorf("structural: compile ast-grep module: %w", err)
			return
		}
		m.compiled = compiled
		m.idle = make(chan *instance, m.poolSize())
	})
	return m.initErr
}

func (m *Matcher) newInstance(ctx context.Context) (*instance, error) {
	name := fmt.Sprintf("astgrep-%d", m.nextName.Add(1))
	mod, err := m.rt.InstantiateModule(ctx, m.compiled, wazero.NewModuleConfig().
		WithName(name).
		WithStartFunctions()) // reactor module: no _start
	if err != nil {
		return nil, fmt.Errorf("structural: instantiate module: %w", err)
	}
	if initFn := mod.ExportedFunction("_initialize"); initFn != nil {
		if _, err := initFn.Call(ctx); err != nil {
			_ = mod.Close(ctx)
			return nil, fmt.Errorf("structural: _initialize: %w", err)
		}
	}
	in := &instance{
		mod:        mod,
		fnAlloc:    mod.ExportedFunction("sg_alloc"),
		fnDealloc:  mod.ExportedFunction("sg_dealloc"),
		fnCompile:  mod.ExportedFunction("sg_compile"),
		fnMatch:    mod.ExportedFunction("sg_match"),
		fnFree:     mod.ExportedFunction("sg_free_pattern"),
		fnLastErr:  mod.ExportedFunction("sg_last_error"),
		fnMeta:     mod.ExportedFunction("sg_meta"),
		patternIDs: make(map[string]uint32),
	}
	if in.fnAlloc == nil || in.fnDealloc == nil || in.fnCompile == nil ||
		in.fnMatch == nil || in.fnFree == nil || in.fnLastErr == nil || in.fnMeta == nil {
		_ = mod.Close(ctx)
		return nil, fmt.Errorf("structural: module is missing required exports")
	}
	return in, nil
}

func (m *Matcher) acquire(ctx context.Context) (*instance, error) {
	if err := m.init(ctx); err != nil {
		return nil, err
	}
	select {
	case in := <-m.idle:
		return in, nil
	default:
	}
	if int(m.created.Add(1)) <= m.poolSize() {
		in, err := m.newInstance(ctx)
		if err != nil {
			m.created.Add(-1)
			return nil, err
		}
		return in, nil
	}
	m.created.Add(-1)
	select {
	case in := <-m.idle:
		return in, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (m *Matcher) release(in *instance) {
	select {
	case m.idle <- in:
	default:
		// Pool full (shouldn't happen); drop the instance.
		_ = in.mod.Close(context.Background())
		m.created.Add(-1)
	}
}

// discard removes a (possibly trapped or context-poisoned) instance from
// circulation instead of returning it to the pool.
func (m *Matcher) discard(in *instance) {
	_ = in.mod.Close(context.Background())
	m.created.Add(-1)
}

// Close releases the runtime and all instances.
func (m *Matcher) Close(ctx context.Context) error {
	if m.rt == nil {
		return nil
	}
	return m.rt.Close(ctx)
}

// Validate compiles the pattern for the given canonical language, returning
// the shim's parse error when the pattern is not valid code.
func (m *Matcher) Validate(ctx context.Context, pattern, lang string) error {
	in, err := m.acquire(ctx)
	if err != nil {
		return err
	}
	_, err = in.compilePattern(ctx, pattern, lang)
	if err != nil {
		if _, ok := err.(*shimError); ok {
			// Clean shim-level error (invalid pattern): the instance is fine.
			m.release(in)
		} else {
			// Trap/context cancellation: instance state is suspect.
			m.discard(in)
		}
		return err
	}
	m.release(in)
	return nil
}

// MatchBytes matches a compiled pattern against one file's contents.
// maxMatches (0 = unlimited) caps the number of matches; the second return
// reports whether the cap truncated results.
func (m *Matcher) MatchBytes(ctx context.Context, pattern, lang string, src []byte, maxMatches int) ([]WasmMatch, bool, error) {
	in, err := m.acquire(ctx)
	if err != nil {
		return nil, false, err
	}
	out, err := in.match(ctx, pattern, lang, src, maxMatches)
	if err != nil {
		if _, ok := err.(*shimError); ok {
			// Clean shim-level error (e.g. non-UTF-8 source): instance fine.
			m.release(in)
		} else {
			// Trap/context cancellation: instance state is suspect.
			m.discard(in)
		}
		return nil, false, err
	}
	m.release(in)
	return out.Matches, out.Truncated, nil
}

// Languages reports the languages bundled in the WASM module.
func (m *Matcher) Languages(ctx context.Context) ([]string, error) {
	in, err := m.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer m.release(in)
	res, err := in.fnMeta.Call(ctx)
	if err != nil {
		return nil, fmt.Errorf("structural: sg_meta: %w", err)
	}
	raw, err := in.readPacked(res[0])
	if err != nil {
		return nil, err
	}
	var meta struct {
		AstGrep   string   `json:"astGrep"`
		Languages []string `json:"languages"`
	}
	if err := json.Unmarshal(raw, &meta); err != nil {
		return nil, fmt.Errorf("structural: decode sg_meta: %w", err)
	}
	return meta.Languages, nil
}

// shimError is an error reported cleanly by the shim (pattern syntax,
// unsupported language, non-UTF-8 source) as opposed to a runtime trap.
type shimError struct{ msg string }

func (e *shimError) Error() string { return e.msg }

func (in *instance) lastError(ctx context.Context) error {
	res, err := in.fnLastErr.Call(ctx)
	if err != nil {
		return fmt.Errorf("structural: sg_last_error: %w", err)
	}
	raw, err := in.readPacked(res[0])
	if err != nil {
		return err
	}
	if len(raw) == 0 {
		return &shimError{msg: "structural: unknown shim error"}
	}
	return &shimError{msg: string(raw)}
}

func (in *instance) writeBuf(ctx context.Context, b []byte) (uint32, error) {
	res, err := in.fnAlloc.Call(ctx, uint64(len(b)))
	if err != nil {
		return 0, fmt.Errorf("structural: sg_alloc: %w", err)
	}
	ptr := uint32(res[0])
	if len(b) > 0 && !in.mod.Memory().Write(ptr, b) {
		return 0, fmt.Errorf("structural: write to module memory failed")
	}
	return ptr, nil
}

func (in *instance) freeBuf(ctx context.Context, ptr uint32, size int) {
	_, _ = in.fnDealloc.Call(ctx, uint64(ptr), uint64(size))
}

// readPacked decodes the shim's packed ptr<<32|len return value and copies
// the referenced module memory.
func (in *instance) readPacked(v uint64) ([]byte, error) {
	ptr := uint32(v >> 32)
	size := uint32(v)
	if size == 0 {
		return nil, nil
	}
	view, ok := in.mod.Memory().Read(ptr, size)
	if !ok {
		return nil, fmt.Errorf("structural: read from module memory failed")
	}
	out := make([]byte, size)
	copy(out, view)
	return out, nil
}

func (in *instance) compilePattern(ctx context.Context, pattern, lang string) (uint32, error) {
	key := lang + "\x00" + pattern
	if id, ok := in.patternIDs[key]; ok {
		return id, nil
	}
	// Bound the per-instance pattern cache; searches typically reuse one
	// pattern across thousands of files, so this is rarely hit.
	if len(in.patternIDs) >= 64 {
		for k, id := range in.patternIDs {
			_, _ = in.fnFree.Call(ctx, uint64(id))
			delete(in.patternIDs, k)
		}
	}
	patPtr, err := in.writeBuf(ctx, []byte(pattern))
	if err != nil {
		return 0, err
	}
	defer in.freeBuf(ctx, patPtr, len(pattern))
	langPtr, err := in.writeBuf(ctx, []byte(lang))
	if err != nil {
		return 0, err
	}
	defer in.freeBuf(ctx, langPtr, len(lang))
	res, err := in.fnCompile.Call(ctx, uint64(patPtr), uint64(len(pattern)), uint64(langPtr), uint64(len(lang)))
	if err != nil {
		return 0, fmt.Errorf("structural: sg_compile: %w", err)
	}
	id := uint32(res[0])
	if id == 0 {
		return 0, in.lastError(ctx)
	}
	in.patternIDs[key] = id
	return id, nil
}

func (in *instance) match(ctx context.Context, pattern, lang string, src []byte, maxMatches int) (matchesOut, error) {
	var out matchesOut
	id, err := in.compilePattern(ctx, pattern, lang)
	if err != nil {
		return out, err
	}
	srcPtr, err := in.writeBuf(ctx, src)
	if err != nil {
		return out, err
	}
	defer in.freeBuf(ctx, srcPtr, len(src))
	if maxMatches < 0 {
		maxMatches = 0
	}
	res, err := in.fnMatch.Call(ctx, uint64(id), uint64(srcPtr), uint64(len(src)), uint64(maxMatches))
	if err != nil {
		return out, fmt.Errorf("structural: sg_match: %w", err)
	}
	if res[0] == 0 {
		return out, in.lastError(ctx)
	}
	raw, err := in.readPacked(res[0])
	if err != nil {
		return out, err
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, fmt.Errorf("structural: decode matches: %w", err)
	}
	return out, nil
}
