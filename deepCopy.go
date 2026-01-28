package deepcopy

import (
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"unsafe"

	_ "unsafe" // 用于 linkname
)

//go:linkname runtimeMemmove runtime.memmove
func runtimeMemmove(to, from unsafe.Pointer, n uintptr)

// visitKey 用于循环引用检测（仅 Ptr/Map/Chan）
type visitKey struct {
	ptr uintptr
	typ reflect.Type
}

// copierCache 普通 map
type copierCache map[reflect.Type]copierFn

type copierFn func(src reflect.Value, visited map[visitKey]reflect.Value, c *Copier) reflect.Value

// Copier 配置选项
type Copier struct {
	useCOW    bool
	cache     atomic.Pointer[copierCache] // COW 模式使用
	muCache   sync.RWMutex                // Mutex 模式使用（COW 模式也复用它处理写）
	mapCache  copierCache                 // Mutex 模式使用
	cacheInit sync.Once                   // Mutex 模式延迟初始化

	handleCycle    bool
	copyUnexported bool
}

// New 创建 Copier（默认 COW 模式，适合类型 < 1000 的热应用）
func New() *Copier {
	c := &Copier{
		useCOW:         true,
		handleCycle:    true,
		copyUnexported: false,
	}
	empty := make(copierCache)
	c.cache.Store(&empty)
	return c
}

// NewHighVolume 创建 Copier（Mutex 模式，适合类型 > 1000 或冷启动场景）
func NewHighVolume() *Copier {
	return &Copier{
		useCOW:         false,
		handleCycle:    true,
		copyUnexported: false,
		mapCache:       nil, // 延迟初始化
	}
}

func (c *Copier) SetCopyUnexported(enable bool) *Copier {
	c.copyUnexported = enable
	return c
}

func (c *Copier) SetHandleCycle(enable bool) *Copier {
	c.handleCycle = enable
	return c
}

// Copy 执行深拷贝，语义：Copy(dst, src)
func (c *Copier) Copy(dst, src interface{}) error {
	if dst == nil || src == nil {
		return fmt.Errorf("dst and src must be non-nil")
	}

	dstVal := reflect.ValueOf(dst)
	if dstVal.Kind() != reflect.Ptr || dstVal.IsNil() {
		return fmt.Errorf("dst must be a non-nil pointer, got %T", dst)
	}
	dstElem := dstVal.Elem()

	srcVal := reflect.ValueOf(src)
	var srcElem reflect.Value

	if srcVal.Kind() == reflect.Ptr {
		if srcVal.IsNil() {
			dstElem.Set(reflect.Zero(dstElem.Type()))
			return nil
		}
		srcElem = srcVal.Elem()
	} else {
		srcElem = srcVal
	}

	if srcElem.Type() != dstElem.Type() {
		return fmt.Errorf("type mismatch: src=%v, dst=%v", srcElem.Type(), dstElem.Type())
	}

	visited := make(map[visitKey]reflect.Value)
	copied := c.deepCopy(srcElem, visited)
	dstElem.Set(copied)
	return nil
}

var (
	globalCopier     *Copier
	globalCopierOnce sync.Once
)

// Clone 便捷函数，使用全局单例 Copier（默认 COW 模式）。
// 注意：如果你的应用需要处理超过 1000 种不同的动态类型，建议使用
// CloneHighVolume 或者手动创建 NewHighVolume() 实例以避免缓存预热时的 CPU 峰值。
func Clone(src interface{}) (interface{}, error) {
	if src == nil {
		return nil, nil
	}

	globalCopierOnce.Do(func() {
		globalCopier = New()
	})

	srcVal := reflect.ValueOf(src)
	visited := make(map[visitKey]reflect.Value)
	return globalCopier.deepCopy(srcVal, visited).Interface(), nil
}

// CloneHighVolume 使用 Mutex 模式全局 Copier
func CloneHighVolume(src interface{}) (interface{}, error) {
	if src == nil {
		return nil, nil
	}

	globalCopierOnce.Do(func() {
		globalCopier = NewHighVolume()
	})

	srcVal := reflect.ValueOf(src)
	visited := make(map[visitKey]reflect.Value)
	return globalCopier.deepCopy(srcVal, visited).Interface(), nil
}

func (c *Copier) deepCopy(src reflect.Value, visited map[visitKey]reflect.Value) reflect.Value {
	fn := c.getCopier(src.Type())
	return fn(src, visited, c)
}

func (c *Copier) getCopier(t reflect.Type) copierFn {
	if c.useCOW {
		return c.getCopierCOW(t)
	}
	return c.getCopierMutex(t)
}

// getCopierCOW: Copy-on-Write 策略，读路径无锁
func (c *Copier) getCopierCOW(t reflect.Type) copierFn {
	// 快路径：原子读
	if m := c.cache.Load(); m != nil {
		if fn, ok := (*m)[t]; ok {
			return fn
		}
	}

	// 慢路径：加锁更新（复用 muCache 的写锁）
	c.muCache.Lock()
	defer c.muCache.Unlock()

	// 双检
	if m := c.cache.Load(); m != nil {
		if fn, ok := (*m)[t]; ok {
			return fn
		}
	}

	// 生成新 copier
	fn := c.generateCopier(t)

	// COW: 拷贝旧 map 并添加新条目
	oldPtr := c.cache.Load()
	newCache := make(copierCache, len(*oldPtr)+1)
	for k, v := range *oldPtr {
		newCache[k] = v
	}
	newCache[t] = fn
	c.cache.Store(&newCache)

	return fn
}

// getCopierMutex: 原地更新策略，避免 O(N^2) 预热
func (c *Copier) getCopierMutex(t reflect.Type) copierFn {
	// 延迟初始化 map
	c.cacheInit.Do(func() {
		c.mapCache = make(copierCache)
	})

	// 快路径：RLock 读
	c.muCache.RLock()
	if fn, ok := c.mapCache[t]; ok {
		c.muCache.RUnlock()
		return fn
	}
	c.muCache.RUnlock()

	// 慢路径：Lock 写
	c.muCache.Lock()
	defer c.muCache.Unlock()

	// 双检
	if fn, ok := c.mapCache[t]; ok {
		return fn
	}

	fn := c.generateCopier(t)
	c.mapCache[t] = fn
	return fn
}

func (c *Copier) generateCopier(t reflect.Type) copierFn {
	switch t.Kind() {
	case reflect.Ptr:
		return c.makePtrCopier(t)
	case reflect.Struct:
		return c.makeStructCopier(t)
	case reflect.Slice:
		return c.makeSliceCopier(t)
	case reflect.Array:
		return c.makeArrayCopier(t)
	case reflect.Map:
		return c.makeMapCopier(t)
	case reflect.Interface:
		return c.makeInterfaceCopier(t)
	default:
		return func(src reflect.Value, visited map[visitKey]reflect.Value, c *Copier) reflect.Value {
			return src
		}
	}
}

func (c *Copier) makePtrCopier(t reflect.Type) copierFn {
	elemType := t.Elem()
	return func(src reflect.Value, visited map[visitKey]reflect.Value, c *Copier) reflect.Value {
		if src.IsNil() {
			return reflect.Zero(t)
		}
		ptr := src.Pointer()
		key := visitKey{ptr: ptr, typ: t}
		if c.handleCycle {
			if cached, ok := visited[key]; ok {
				return cached
			}
		}
		dst := reflect.New(elemType)
		if c.handleCycle {
			visited[key] = dst
		}
		elemFn := c.getCopier(elemType)
		copiedElem := elemFn(src.Elem(), visited, c)
		dst.Elem().Set(copiedElem)
		return dst
	}
}

func (c *Copier) makeStructCopier(t reflect.Type) copierFn {
	type fieldMeta struct {
		index     int
		offset    uintptr
		copier    copierFn
		canSet    bool
		fieldType reflect.Type
	}

	fields := make([]fieldMeta, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		fields = append(fields, fieldMeta{
			index:     i,
			offset:    f.Offset,
			copier:    c.getCopier(f.Type),
			canSet:    f.PkgPath == "",
			fieldType: f.Type,
		})
	}

	return func(src reflect.Value, visited map[visitKey]reflect.Value, c *Copier) reflect.Value {
		dst := reflect.New(t).Elem()

		for _, fm := range fields {
			if fm.canSet {
				srcField := src.Field(fm.index)
				copied := fm.copier(srcField, visited, c)
				dst.Field(fm.index).Set(copied)
			} else if c.copyUnexported {
				srcPtr := unsafe.Pointer(src.UnsafeAddr() + fm.offset)
				srcField := reflect.NewAt(fm.fieldType, srcPtr).Elem()

				copied := fm.copier(srcField, visited, c)

				dstPtr := unsafe.Pointer(dst.UnsafeAddr() + fm.offset)
				runtimeMemmove(dstPtr, unsafe.Pointer(copied.UnsafeAddr()), fm.fieldType.Size())
			}
		}
		return dst
	}
}

// 修正 1: reflex.Value -> reflect.Value
func (c *Copier) makeSliceCopier(t reflect.Type) copierFn {
	elemType := t.Elem()
	elemFn := c.getCopier(elemType)
	isPOD := isPlainOldData(elemType)

	return func(src reflect.Value, visited map[visitKey]reflect.Value, c *Copier) reflect.Value {
		if src.IsNil() {
			return reflect.Zero(t)
		}
		n := src.Len()
		dst := reflect.MakeSlice(t, n, src.Cap())

		if isPOD {
			reflect.Copy(dst, src)
			return dst
		}

		for i := 0; i < n; i++ {
			elem := src.Index(i)
			copied := elemFn(elem, visited, c)
			dst.Index(i).Set(copied)
		}
		return dst
	}
}

func (c *Copier) makeArrayCopier(t reflect.Type) copierFn {
	elemType := t.Elem()
	length := t.Len()
	elemFn := c.getCopier(elemType)
	isPOD := isPlainOldData(elemType)

	return func(src reflect.Value, visited map[visitKey]reflect.Value, c *Copier) reflect.Value {
		dst := reflect.New(t).Elem()
		if isPOD {
			reflect.Copy(dst.Slice(0, length), src.Slice(0, length))
			return dst
		}
		for i := 0; i < length; i++ {
			copied := elemFn(src.Index(i), visited, c)
			dst.Index(i).Set(copied)
		}
		return dst
	}
}

func (c *Copier) makeMapCopier(t reflect.Type) copierFn {
	keyFn := c.getCopier(t.Key())
	elemFn := c.getCopier(t.Elem())

	return func(src reflect.Value, visited map[visitKey]reflect.Value, c *Copier) reflect.Value {
		if src.IsNil() {
			return reflect.Zero(t)
		}
		if c.handleCycle {
			ptr := src.Pointer()
			key := visitKey{ptr: ptr, typ: t}
			if cached, ok := visited[key]; ok {
				return cached
			}
		}
		dst := reflect.MakeMapWithSize(t, src.Len())
		if c.handleCycle {
			visited[visitKey{ptr: src.Pointer(), typ: t}] = dst
		}
		for _, key := range src.MapKeys() {
			newKey := keyFn(key, visited, c)
			val := src.MapIndex(key)
			newVal := elemFn(val, visited, c)
			dst.SetMapIndex(newKey, newVal)
		}
		return dst
	}
}

func (c *Copier) makeInterfaceCopier(t reflect.Type) copierFn {
	return func(src reflect.Value, visited map[visitKey]reflect.Value, c *Copier) reflect.Value {
		if src.IsNil() {
			return reflect.Zero(t)
		}
		actual := src.Elem()
		return c.deepCopy(actual, visited)
	}
}

func isPlainOldData(t reflect.Type) bool {
	switch t.Kind() {
	case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64, reflect.Complex64, reflect.Complex128:
		return true
	case reflect.Array:
		return isPlainOldData(t.Elem())
	default:
		return false
	}
}

// Copy 便捷函数（使用全局单例）
func Copy(dst, src interface{}) error {
	globalCopierOnce.Do(func() {
		globalCopier = New()
	})
	return globalCopier.Copy(dst, src)
}
