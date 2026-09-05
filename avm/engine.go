package avm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

var (
	ErrOutOfGas      = errors.New("avm: out of gas")
	ErrReadOnlyWrite = errors.New("avm: write attempted in read-only execution")
)

const (
	GasStateRead   uint64 = 25
	GasStateWrite  uint64 = 100
	GasInputRead   uint64 = 10
	GasOutputWrite uint64 = 10
)

type ExecutionRequest struct {
	ContractID [32]byte
	Code       []byte
	Entry      string
	Input      []byte
	GasLimit   uint64
	ReadOnly   bool
	AccessList []Access
}

type Access struct {
	Key   []byte
	Write bool
}

type ExecutionResult struct {
	GasUsed      uint64
	ReturnData   []byte
	StateWrites  int
	ModuleReport ValidationReport
}

// Engine executes deterministic, sandboxed AVM v1 modules. Compilation output
// is shared through wazero's version-keyed cache, while each call receives a
// fresh runtime and host module so execution sessions cannot leak state.
type Engine struct {
	store Store
	cache wazero.CompilationCache
	mu    sync.Mutex
}

func NewEngine(store Store) *Engine {
	if store == nil {
		store = NewMemoryStore()
	}
	return &Engine{store: store, cache: wazero.NewCompilationCache()}
}

func (e *Engine) Close(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.cache == nil {
		return nil
	}
	err := e.cache.Close(ctx)
	e.cache = nil
	return err
}

func (e *Engine) Execute(ctx context.Context, request ExecutionRequest) (ExecutionResult, error) {
	var result ExecutionResult
	if request.Entry == "" {
		request.Entry = "run"
	}
	if len(request.Input) > MaxInputSize {
		return result, fmt.Errorf("avm: input is %d bytes, max %d", len(request.Input), MaxInputSize)
	}
	report, err := ValidateModule(request.Code)
	if err != nil {
		return result, err
	}
	result.ModuleReport = report
	session := &executionSession{
		request: request,
		state:   newTransactionState(e.store, request.ContractID),
		access:  make(map[string]bool, len(request.AccessList)),
	}
	for _, access := range request.AccessList {
		if len(access.Key) == 0 || len(access.Key) > MaxStateKeySize {
			return result, fmt.Errorf("avm: access key must be 1..%d bytes", MaxStateKeySize)
		}
		key := string(access.Key)
		if _, exists := session.access[key]; exists {
			return result, fmt.Errorf("avm: duplicate access key")
		}
		session.access[key] = access.Write
	}
	if !session.charge(report.StaticGas + uint64(len(request.Input))) {
		result.GasUsed = request.GasLimit
		return result, ErrOutOfGas
	}

	e.mu.Lock()
	cache := e.cache
	e.mu.Unlock()
	if cache == nil {
		return result, fmt.Errorf("avm: engine is closed")
	}
	runtimeConfig := wazero.NewRuntimeConfigCompiler().
		WithCoreFeatures(api.CoreFeaturesV2).
		WithMemoryLimitPages(MaxMemoryPages).
		WithCompilationCache(cache).
		WithCloseOnContextDone(true).
		WithDebugInfoEnabled(false)
	runtime := wazero.NewRuntimeWithConfig(ctx, runtimeConfig)
	defer runtime.Close(ctx)

	if err := instantiateHost(ctx, runtime, session); err != nil {
		return result, fmt.Errorf("avm: instantiate host ABI: %w", err)
	}
	compiled, err := runtime.CompileModule(ctx, request.Code)
	if err != nil {
		return result, fmt.Errorf("avm: compile module: %w", err)
	}
	defer compiled.Close(ctx)

	instance, err := runtime.InstantiateModule(ctx, compiled,
		wazero.NewModuleConfig().WithName("contract").WithStartFunctions())
	if err != nil {
		return result, fmt.Errorf("avm: instantiate module: %w", err)
	}
	defer instance.Close(ctx)

	entry := instance.ExportedFunction(request.Entry)
	if entry == nil {
		return result, fmt.Errorf("avm: entry function %q is not exported", request.Entry)
	}
	definition := entry.Definition()
	if len(definition.ParamTypes()) != 0 || len(definition.ResultTypes()) != 0 {
		return result, fmt.Errorf("avm: entry function %q must have signature () -> ()", request.Entry)
	}
	if _, err := entry.Call(ctx); err != nil {
		result.GasUsed = session.gasUsed
		if session.err != nil {
			return result, session.err
		}
		return result, fmt.Errorf("avm: contract trapped: %w", err)
	}
	result.GasUsed = session.gasUsed
	result.ReturnData = bytes.Clone(session.output)
	writes := session.state.orderedWrites()
	result.StateWrites = len(writes)
	if request.ReadOnly {
		if len(writes) != 0 {
			return result, ErrReadOnlyWrite
		}
		return result, nil
	}
	if err := e.store.Apply(writes); err != nil {
		return result, fmt.Errorf("avm: commit state: %w", err)
	}
	return result, nil
}

type executionSession struct {
	request ExecutionRequest
	state   *transactionState
	gasUsed uint64
	output  []byte
	err     error
	access  map[string]bool
}

func (s *executionSession) charge(amount uint64) bool {
	if amount > s.request.GasLimit-s.gasUsed {
		s.gasUsed = s.request.GasLimit
		s.err = ErrOutOfGas
		return false
	}
	s.gasUsed += amount
	return true
}

func (s *executionSession) fail(ctx context.Context, module api.Module, err error) {
	if s.err == nil {
		s.err = err
	}
	_ = module.CloseWithExitCode(ctx, 1)
}

func instantiateHost(ctx context.Context, runtime wazero.Runtime, session *executionSession) error {
	builder := runtime.NewHostModuleBuilder("apro")
	builder.NewFunctionBuilder().WithFunc(func(ctx context.Context, module api.Module, amount uint64) {
		if !session.charge(amount) {
			session.fail(ctx, module, ErrOutOfGas)
		}
	}).Export("consume_gas")
	builder.NewFunctionBuilder().WithFunc(func(ctx context.Context, module api.Module, keyPtr, keyLen, outPtr, outCap uint32) uint32 {
		if !session.charge(GasStateRead + uint64(keyLen)) {
			session.fail(ctx, module, ErrOutOfGas)
			return ^uint32(0)
		}
		key, ok := readMemory(module, keyPtr, keyLen)
		if !ok || len(key) == 0 || len(key) > MaxStateKeySize {
			session.fail(ctx, module, fmt.Errorf("avm: invalid state key"))
			return ^uint32(0)
		}
		if _, declared := session.access[string(key)]; !declared {
			session.fail(ctx, module, fmt.Errorf("avm: undeclared state read"))
			return ^uint32(0)
		}
		value, found, err := session.state.get(key)
		if err != nil {
			session.fail(ctx, module, fmt.Errorf("avm: state read: %w", err))
			return ^uint32(0)
		}
		if !found {
			return ^uint32(0)
		}
		if uint32(len(value)) > outCap || !module.Memory().Write(outPtr, value) {
			session.fail(ctx, module, fmt.Errorf("avm: state read output buffer too small"))
			return ^uint32(0)
		}
		return uint32(len(value))
	}).Export("state_read")
	builder.NewFunctionBuilder().WithFunc(func(ctx context.Context, module api.Module, keyPtr, keyLen, valuePtr, valueLen uint32) uint32 {
		if !session.charge(GasStateWrite + uint64(keyLen) + uint64(valueLen)) {
			session.fail(ctx, module, ErrOutOfGas)
			return 1
		}
		if session.request.ReadOnly {
			session.fail(ctx, module, ErrReadOnlyWrite)
			return 1
		}
		key, keyOK := readMemory(module, keyPtr, keyLen)
		value, valueOK := readMemory(module, valuePtr, valueLen)
		if !keyOK || !valueOK || len(key) == 0 || len(key) > MaxStateKeySize || len(value) > MaxStateValue {
			session.fail(ctx, module, fmt.Errorf("avm: invalid state write"))
			return 1
		}
		writeAllowed, declared := session.access[string(key)]
		if !declared || !writeAllowed {
			session.fail(ctx, module, fmt.Errorf("avm: undeclared state write"))
			return 1
		}
		session.state.put(key, value)
		return 0
	}).Export("state_write")
	builder.NewFunctionBuilder().WithFunc(func(ctx context.Context, module api.Module, outPtr, outCap uint32) uint32 {
		if !session.charge(GasInputRead + uint64(len(session.request.Input))) {
			session.fail(ctx, module, ErrOutOfGas)
			return ^uint32(0)
		}
		if uint32(len(session.request.Input)) > outCap || !module.Memory().Write(outPtr, session.request.Input) {
			session.fail(ctx, module, fmt.Errorf("avm: input buffer too small"))
			return ^uint32(0)
		}
		return uint32(len(session.request.Input))
	}).Export("input_read")
	builder.NewFunctionBuilder().WithFunc(func(ctx context.Context, module api.Module, ptr, length uint32) uint32 {
		if !session.charge(GasOutputWrite + uint64(length)) {
			session.fail(ctx, module, ErrOutOfGas)
			return 1
		}
		if length > MaxStateValue {
			session.fail(ctx, module, fmt.Errorf("avm: output exceeds %d bytes", MaxStateValue))
			return 1
		}
		output, ok := readMemory(module, ptr, length)
		if !ok {
			session.fail(ctx, module, fmt.Errorf("avm: invalid output memory"))
			return 1
		}
		session.output = bytes.Clone(output)
		return 0
	}).Export("output_write")
	_, err := builder.Instantiate(ctx)
	return err
}

func readMemory(module api.Module, offset, length uint32) ([]byte, bool) {
	memory := module.Memory()
	if memory == nil {
		return nil, false
	}
	value, ok := memory.Read(offset, length)
	if !ok {
		return nil, false
	}
	return bytes.Clone(value), true
}
