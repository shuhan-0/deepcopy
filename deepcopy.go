package deepcopy

import (
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"unsafe"

	_ "unsafe"
)

//go:linkname runtimeMemmove runtime.memmove
func runtimeMemmove(to, from unsafe.Pointer, n uintptr)

// visitKey 用于循环引用检测（仅 Ptr/Map/Chan）
type visitKey struct {
	ptr uintptr
	typ reflect.Type
}

// copierKind 使用 uint8 压缩内存
type copierKind uint8

const (
	kindBasic copierKind = iota
	kindPtr
	kindSlice
	kindArray
	kindMap
	kindStruct
	kindInterface
	kindUnsupported
)

// typeCopier 压缩布局，64位系统下从 72 字节降至 48 字节
// 顺序：大字段在前，小字段在后，减少 padding
type typeCopier struct {
	// 8 字节对齐字段
	typ  reflect.Type
	elem *typeCopier // Slice/Array/Ptr 的元素
	key  *typeCopier // Map 的 key

	// 结构体专用，nil 表示非结构体（节省 8 字节 nil 指针）
	fields *[]fieldCopier // 使用指针指向切片，减少空结构体的内存浪费

	// 4 字节字段
	arrayLen int32 // Array 长度，int32 足够（最大 2^31-1）

	// 1 字节字段
	kind  copierKind
	isPOD bool
}

// fieldCopier 结构体字段描述符，32 字节（紧凑布局）
type fieldCopier struct {
	copier    *typeCopier  // 8 字节
	fieldType reflect.Type // 8 字节
	offset    uintptr      // 8 字节
	index     int32        // 4 字节
	canSet    bool         // 1 字节
	_         [3]byte      // padding 到 32 字节
}

// visitedPool 复用 visited map，减少 GC 压力
var visitedPool = sync.Pool{
	New: func() interface{} {
		return make(map[visitKey]reflect.Value, 16) // 预分配初始容量
	},
}

func acquireVisited() map[visitKey]reflect.Value {
	m := visitedPool.Get().(map[visitKey]reflect.Value)
	clear(m) // Go 1.21+，快速清空 map 保留容量
	return m
}

func releaseVisited(m map[visitKey]reflect.Value) {
	if len(m) < 10000 { // 防止极端情况下的内存泄漏
		visitedPool.Put(m)
	}
}

// copierCache 定义
type copierCache map[reflect.Type]*typeCopier

// Copier 配置
type Copier struct {
	useCOW         bool
	cache          atomic.Pointer[copierCache]
	muCache        sync.RWMutex
	mapCache       copierCache
	cacheInit      sync.Once
	handleCycle    bool
	copyUnexported bool
}

// New 创建 Copier（COW 模式，适合类型 < 1000）
func New() *Copier {
	c := &Copier{
		useCOW:         true,
		handleCycle:    true,
		copyUnexported: false,
	}
	empty := make(copierCache, 64) // 预分配初始容量
	c.cache.Store(&empty)
	return c
}

// NewHighVolume 创建 Copier（Mutex 模式，适合类型 > 1000）
func NewHighVolume() *Copier {
	return &Copier{
		useCOW:         false,
		handleCycle:    true,
		copyUnexported: false,
		mapCache:       nil,
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

// Copy 执行深拷贝
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

	var visited map[visitKey]reflect.Value
	if c.handleCycle {
		visited = acquireVisited()
		defer releaseVisited(visited)
	}

	tc := c.getTypeCopier(srcElem.Type())
	copied := tc.copy(srcElem, visited, c)
	dstElem.Set(copied)
	return nil
}

// Clone 深拷贝并返回新对象
func Clone(src interface{}) (interface{}, error) {
	globalCopierOnce.Do(func() {
		globalCopier = New()
	})
	return globalCopier.Clone(src)
}

// Clone 方法
func (c *Copier) Clone(src interface{}) (interface{}, error) {
	if src == nil {
		return nil, nil
	}
	srcVal := reflect.ValueOf(src)
	if srcVal.Kind() == reflect.Ptr && srcVal.IsNil() {
		return nil, nil
	}

	var visited map[visitKey]reflect.Value
	if c.handleCycle {
		visited = acquireVisited()
		defer releaseVisited(visited)
	}

	tc := c.getTypeCopier(srcVal.Type())
	dst := tc.copy(srcVal, visited, c)
	return dst.Interface(), nil
}

var (
	globalCopier     *Copier
	globalCopierOnce sync.Once
)

// getTypeCopier 获取类型处理器
func (c *Copier) getTypeCopier(t reflect.Type) *typeCopier {
	if c.useCOW {
		return c.getTypeCopierCOW(t)
	}
	return c.getTypeCopierMutex(t)
}

// getTypeCopierCOW: COW 模式
func (c *Copier) getTypeCopierCOW(t reflect.Type) *typeCopier {
	// 快路径：无锁读
	if m := c.cache.Load(); m != nil {
		if tc, ok := (*m)[t]; ok {
			return tc
		}
	}

	// 慢路径：加锁
	c.muCache.Lock()

	// 双检
	if m := c.cache.Load(); m != nil {
		if tc, ok := (*m)[t]; ok {
			c.muCache.Unlock()
			return tc
		}
	}

	// 创建占位符（关键：防止递归死锁）
	tc := c.createPlaceholder(t)

	// 发布占位符（让其他 goroutine 可见）
	oldPtr := c.cache.Load()
	newCache := make(copierCache, len(*oldPtr)+1)
	for k, v := range *oldPtr {
		newCache[k] = v
	}
	newCache[t] = tc
	c.cache.Store(&newCache)
	c.muCache.Unlock()

	// 锁外填充（允许递归）
	c.fillTypeCopier(tc, t)

	// 更新为完整版本（可选优化：原地修改，因为 tc 是指针）
	return tc
}

// getTypeCopierMutex: Mutex 模式
func (c *Copier) getTypeCopierMutex(t reflect.Type) *typeCopier {
	c.cacheInit.Do(func() {
		c.mapCache = make(copierCache, 1024) // 预分配
	})

	// 快路径：RLock
	c.muCache.RLock()
	if tc, ok := c.mapCache[t]; ok && tc.isComplete() {
		c.muCache.RUnlock()
		return tc
	}
	c.muCache.RUnlock()

	// 慢路径：Lock
	c.muCache.Lock()

	// 双检
	if tc, ok := c.mapCache[t]; ok && tc.isComplete() {
		c.muCache.Unlock()
		return tc
	}

	// 创建并填充
	tc := c.createPlaceholder(t)
	c.mapCache[t] = tc
	c.muCache.Unlock()

	c.fillTypeCopier(tc, t)
	return tc
}

// createPlaceholder 创建占位符 typeCopier
func (c *Copier) createPlaceholder(t reflect.Type) *typeCopier {
	tc := &typeCopier{
		typ:  t,
		kind: kindFromType(t),
	}

	// 预计算 isPOD（适用于 Slice/Array）
	switch tc.kind {
	case kindSlice, kindArray:
		tc.isPOD = isPlainOldData(t.Elem())
		if tc.kind == kindArray {
			tc.arrayLen = int32(t.Len())
		}
	}

	return tc
}

// isComplete 检查 typeCopier 是否已填充完成
func (tc *typeCopier) isComplete() bool {
	switch tc.kind {
	case kindBasic, kindUnsupported, kindInterface:
		return true
	case kindPtr, kindSlice, kindArray:
		return tc.elem != nil
	case kindMap:
		return tc.key != nil && tc.elem != nil
	case kindStruct:
		return tc.fields != nil
	}
	return false
}

// fillTypeCopier 填充 typeCopier 的递归字段（锁外执行）
func (c *Copier) fillTypeCopier(tc *typeCopier, t reflect.Type) {
	switch tc.kind {
	case kindPtr:
		tc.elem = c.getTypeCopier(t.Elem())
	case kindSlice:
		tc.elem = c.getTypeCopier(t.Elem())
	case kindArray:
		tc.elem = c.getTypeCopier(t.Elem())
	case kindMap:
		tc.key = c.getTypeCopier(t.Key())
		tc.elem = c.getTypeCopier(t.Elem())
	case kindStruct:
		fields := make([]fieldCopier, t.NumField())
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			fields[i] = fieldCopier{
				index:     int32(i),
				offset:    f.Offset,
				canSet:    f.PkgPath == "",
				copier:    c.getTypeCopier(f.Type),
				fieldType: f.Type,
			}
		}
		tc.fields = &fields
	}
}

func kindFromType(t reflect.Type) copierKind {
	switch t.Kind() {
	case reflect.Ptr:
		return kindPtr
	case reflect.Slice:
		return kindSlice
	case reflect.Array:
		return kindArray
	case reflect.Map:
		return kindMap
	case reflect.Struct:
		return kindStruct
	case reflect.Interface:
		return kindInterface
	case reflect.Chan, reflect.Func:
		return kindUnsupported
	default:
		return kindBasic
	}
}

// copy 执行拷贝（使用指针接收者，避免值拷贝）
func (tc *typeCopier) copy(src reflect.Value, visited map[visitKey]reflect.Value, c *Copier) reflect.Value {
	switch tc.kind {
	case kindBasic:
		return src // 零开销
	case kindPtr:
		return tc.copyPtr(src, visited, c)
	case kindSlice:
		return tc.copySlice(src, visited, c)
	case kindArray:
		return tc.copyArray(src, visited, c)
	case kindMap:
		return tc.copyMap(src, visited, c)
	case kindStruct:
		return tc.copyStruct(src, visited, c)
	case kindInterface:
		return tc.copyInterface(src, visited, c)
	case kindUnsupported:
		return reflect.Zero(tc.typ)
	default:
		return src
	}
}

func (tc *typeCopier) copyPtr(src reflect.Value, visited map[visitKey]reflect.Value, c *Copier) reflect.Value {
	if src.IsNil() {
		return reflect.Zero(tc.typ)
	}

	if c.handleCycle && visited != nil {
		ptr := src.Pointer()
		key := visitKey{ptr: ptr, typ: tc.typ}
		if cached, ok := visited[key]; ok {
			return cached
		}

		dst := reflect.New(tc.typ.Elem())
		visited[key] = dst

		copiedElem := tc.elem.copy(src.Elem(), visited, c)
		dst.Elem().Set(copiedElem)
		return dst
	}

	dst := reflect.New(tc.typ.Elem())
	copiedElem := tc.elem.copy(src.Elem(), visited, c)
	dst.Elem().Set(copiedElem)
	return dst
}

// copySlice: 修复 - 移除 Slice 本身的循环引用检测
func (tc *typeCopier) copySlice(src reflect.Value, visited map[visitKey]reflect.Value, c *Copier) reflect.Value {
	if src.IsNil() {
		return reflect.Zero(tc.typ)
	}

	n := src.Len()
	dst := reflect.MakeSlice(tc.typ, n, src.Cap())

	// POD 快速路径：整块内存拷贝
	if tc.isPOD {
		reflect.Copy(dst, src)
		return dst
	}

	// 非 POD：逐元素深拷贝（元素的循环引用由元素自身的 copy 处理）
	for i := 0; i < n; i++ {
		copied := tc.elem.copy(src.Index(i), visited, c)
		dst.Index(i).Set(copied)
	}
	return dst
}

// copyArray: 修复 - 逐元素复制避免不可寻址问题
func (tc *typeCopier) copyArray(src reflect.Value, visited map[visitKey]reflect.Value, c *Copier) reflect.Value {
	dst := reflect.New(tc.typ).Elem()

	// POD 快速路径：逐元素复制
	if tc.isPOD {
		for i := 0; i < int(tc.arrayLen); i++ {
			dst.Index(i).Set(src.Index(i))
		}
		return dst
	}

	// 非 POD：逐元素
	for i := 0; i < int(tc.arrayLen); i++ {
		copied := tc.elem.copy(src.Index(i), visited, c)
		dst.Index(i).Set(copied)
	}
	return dst
}

func (tc *typeCopier) copyMap(src reflect.Value, visited map[visitKey]reflect.Value, c *Copier) reflect.Value {
	if src.IsNil() {
		return reflect.Zero(tc.typ)
	}

	dst := reflect.MakeMapWithSize(tc.typ, src.Len())

	// 立即注册到 visited（关键：防止递归时无限循环）
	if c.handleCycle && visited != nil {
		ptr := src.Pointer()
		key := visitKey{ptr: ptr, typ: tc.typ}
		if cached, ok := visited[key]; ok {
			return cached
		}
		visited[key] = dst
	}

	for _, key := range src.MapKeys() {
		newKey := tc.key.copy(key, visited, c)
		newVal := tc.elem.copy(src.MapIndex(key), visited, c)
		dst.SetMapIndex(newKey, newVal)
	}
	return dst
}

func (tc *typeCopier) copyStruct(src reflect.Value, visited map[visitKey]reflect.Value, c *Copier) reflect.Value {
	dst := reflect.New(tc.typ).Elem()

	// 快速路径：无可导出字段且未开启 copyUnexported
	if tc.fields == nil || len(*tc.fields) == 0 {
		return dst
	}

	srcCanAddr := src.CanAddr()

	for i := range *tc.fields {
		fc := &(*tc.fields)[i] // 使用指针避免拷贝

		if fc.canSet {
			copied := fc.copier.copy(src.Field(int(fc.index)), visited, c)
			dst.Field(int(fc.index)).Set(copied)
		} else if c.copyUnexported && srcCanAddr {
			// 未导出字段处理
			srcPtr := unsafe.Pointer(src.UnsafeAddr() + fc.offset)
			srcField := reflect.NewAt(fc.fieldType, srcPtr).Elem()

			copied := fc.copier.copy(srcField, visited, c)

			// 确保 copied 可寻址以使用 memmove
			if !copied.CanAddr() {
				// 回退到 Set（极少发生）
				dstPtr := unsafe.Pointer(dst.UnsafeAddr() + fc.offset)
				dstField := reflect.NewAt(fc.fieldType, dstPtr).Elem()
				dstField.Set(copied)
			} else {
				dstPtr := unsafe.Pointer(dst.UnsafeAddr() + fc.offset)
				runtimeMemmove(dstPtr, unsafe.Pointer(copied.UnsafeAddr()), fc.fieldType.Size())
			}
		}
	}
	return dst
}

func (tc *typeCopier) copyInterface(src reflect.Value, visited map[visitKey]reflect.Value, c *Copier) reflect.Value {
	if src.IsNil() {
		return reflect.Zero(tc.typ)
	}

	actual := src.Elem()
	actualType := actual.Type()

	// 获取或创建实际类型的 copier
	actualCopier := c.getTypeCopier(actualType)
	copied := actualCopier.copy(actual, visited, c)

	// 转换回接口类型（如果必要）
	if copied.Type() != tc.typ {
		return copied.Convert(tc.typ)
	}
	return copied
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

// Copy 便捷函数
func Copy(dst, src interface{}) error {
	globalCopierOnce.Do(func() {
		globalCopier = New()
	})
	return globalCopier.Copy(dst, src)
}
