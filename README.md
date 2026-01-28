# deepCopy

[![Go Reference](https://pkg.go.dev/badge/github.com/shuhan-0/deepCopy.svg)](https://pkg.go.dev/github.com/shuhan-0/deepCopy)
[![Go Report Card](https://goreportcard.com/badge/github.com/shuhan-0/deepCopy)](https://goreportcard.com/report/github.com/shuhan-0/deepCopy)

High-performance deep copy library for Go 1.20+ with zero dependencies.

## Features

- **JIT compilation**: Generates type-specific copy functions on first use, zero reflection afterwards
- **Dual cache strategies**: COW (lock-free reads) or HighVolume (O(1) writes)
- **Cyclic reference handling**: Automatic detection for pointers and maps
- **Unexported field support**: Optional unsafe copy of private fields
- **Zero-allocation POD paths**: Direct runtime.memmove for basic types

## Requirements

- Go 1.20 or later

## Installation

```bash
go get github.com/shuhan-0/deepCopy
```

## Usage

### Basic

```go
package main

import (
    "fmt"
    "github.com/shuhan-0/deepCopy"
)

type Config struct {
    Name    string
    Servers []string
    Nested  *Config
}

func main() {
    src := &Config{
        Name:    "production",
        Servers: []string{"srv1", "srv2"},
        Nested:  &Config{Name: "child"},
    }
    src.Nested.Nested = src // cyclic reference

    var dst Config
    if err := deepCopy.Copy(&dst, src); err != nil {
        panic(err)
    }

    fmt.Println(dst.Name)              // "production"
    fmt.Println(dst.Nested.Nested == &dst) // true (cycle preserved)
}
```

### Advanced Options

```go
// HighVolume mode: for >1000 types or dynamic loading
copier := deepCopy.NewHighVolume()

// Copy unexported fields (use with caution)
copier.SetCopyUnexported(true)

// Disable cycle detection (micro-optimization)
copier.SetHandleCycle(false)
```

## API

### Functions

| Function | Description |
|----------|-------------|
| `New() *Copier` | COW mode (default), lock-free reads, best for <1000 types |
| `NewHighVolume() *Copier` | Mutex mode, O(1) writes, best for dynamic type registration |
| `Copy(dst, src interface{}) error` | Deep copy src to dst (dst must be non-nil pointer) |
| `Clone(src interface{}) (interface{}, error)` | Returns deep copy as interface{} (uses global singleton) |

### Methods

- `SetCopyUnexported(bool) *Copier` - Enable copying of unexported fields
- `SetHandleCycle(bool) *Copier` - Enable cyclic reference detection (default: true)

## Performance

| Scenario | Time | vs JSON |
|----------|------|---------|
| Cached struct copy | ~50ns/field | 10-50x faster |
| POD slice (1MB) | ~1ms | 20x faster, 90% less alloc |
| First type registration | ~5μs | N/A (one-time) |

## Safety

- GC-safe: All pointer operations maintain heap traceability
- No stack overflow: Large arrays use chunked copy (64KB blocks)
- Slice independence: Always creates new backing arrays (no shared views)

## Limitations

- `chan` and `func` fields are zeroed (cannot be safely copied)
- Unexported fields skipped by default (enable with `SetCopyUnexported(true)`)

## License

MIT