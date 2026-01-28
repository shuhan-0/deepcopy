package deepcopy

// import (
// 	"fmt"
// 	"reflect"
// 	"sync"
// 	"sync/atomic"
// 	"unsafe"

// 	_ "unsafe" // 用于 linkname
// )

// //go:linkname runtimeMemmove runtime.memmove
// func runtimeMemmove(to, from unsafe.Pointer, n uintptr)

// // visitKey 用于循环引用检测（仅 Ptr/Map/Chan）
// type visitKey struct {
// 	ptr uintptr
// 	typ reflect.Type
// }

// // copierCache 普通 map
// type copierCache map[reflect.Type]copierFn

// type copierFn func(src reflect.Value, visited map[visitKey]reflect.Value, c *Copier) reflect.Value

// // Copier 配置选项
// type Copier struct {
// 	useCOW    bool
// 	cache     atomic.Pointer[copierCache] // COW 模式使用
// 	muCache   sync.RWMutex                // Mutex 模式使用（COW 模式也复用它处理写）
// 	mapCache  copierCache                 // Mutex 模式使用
// 	cacheInit sync.Once                   // Mutex 模式延迟初始化

// 	handleCycle    bool
// 	copyUnexported bool
// }

// // New 创建 Copier（默认 COW 模式，适合类型 < 1000 的热应用）
// func New() *Copier {
// 	c := &Copier{
// 		useCOW:         true,
// 		handleCycle:    true,
// 		copyUnexported: false,
// 	}
// 	empty := make(copierCache)
// 	c.cache.Store(&empty)
// 	return c
// }

// // NewHighVolume 创建 Copier（Mutex 模式，适合类型 > 1000 或冷启动场景）
// func NewHighVolume() *Copier {
// 	return &Copier{
// 		useCOW:         false,
// 		handleCycle:    true,
// 		copyUnexported: false,
// 		mapCache:       nil, // 延迟初始化
// 	}
// }

// func (c *Copier) SetCopyUnexported(enable bool) *Copier {
// 	c.copyUnexported = enable
// 	return c
// }

// func (c *Copier) SetHandleCycle(enable bool) *Copier {
// 	c.handleCycle = enable
// 	return c
// }

// // Copy 执行深拷贝，语义：Copy(dst, src)
// func (c *Copier) Copy(dst, src interface{}) error {
// 	if dst == nil || src == nil {
// 		return fmt.Errorf("dst and src must be non-nil")
// 	}

// 	dstVal := reflect.ValueOf(dst)
// 	if dstVal.Kind() != reflect.Ptr || dstVal.IsNil() {
// 		return fmt.Errorf("dst must be a non-nil pointer, got %T", dst)
// 	}
// 	dstElem := dstVal.Elem()

// 	srcVal := reflect.ValueOf(src)
// 	var srcElem reflect.Value

// 	if srcVal.Kind() == reflect.Ptr {
// 		if srcVal.IsNil() {
// 			dstElem.Set(reflect.Zero(dstElem.Type()))
// 			return nil
// 		}
// 		srcElem = srcVal.Elem()
// 	} else {
// 		srcElem = srcVal
// 	}

// 	if srcElem.Type() != dstElem.Type() {
// 		return fmt.Errorf("type mismatch: src=%v, dst=%v", srcElem.Type(), dstElem.Type())
// 	}

// 	visited := make(map[visitKey]reflect.Value)
// 	copied := c.deepCopy(srcElem, visited)
// 	dstElem.Set(copied)
// 	return nil
// }

// var (
// 	globalCopier     *Copier
// 	globalCopierOnce sync.Once
// )

// // Clone 便捷函数，使用全局单例 Copier（默认 COW 模式）。
// // 注意：如果你的应用需要处理超过 1000 种不同的动态类型，建议使用
// // CloneHighVolume 或者手动创建 NewHighVolume() 实例以避免缓存预热时的 CPU 峰值。
// func Clone(src interface{}) (interface{}, error) {
// 	if src == nil {
// 		return nil, nil
// 	}

// 	globalCopierOnce.Do(func() {
// 		globalCopier = New()
// 	})

// 	srcVal := reflect.ValueOf(src)
// 	visited := make(map[visitKey]reflect.Value)
// 	return globalCopier.deepCopy(srcVal, visited).Interface(), nil
// }

// // CloneHighVolume 使用 Mutex 模式全局 Copier
// func CloneHighVolume(src interface{}) (interface{}, error) {
// 	if src == nil {
// 		return nil, nil
// 	}

// 	globalCopierOnce.Do(func() {
// 		globalCopier = NewHighVolume()
// 	})

// 	srcVal := reflect.ValueOf(src)
// 	visited := make(map[visitKey]reflect.Value)
// 	return globalCopier.deepCopy(srcVal, visited).Interface(), nil
// }

// func (c *Copier) deepCopy(src reflect.Value, visited map[visitKey]reflect.Value) reflect.Value {
// 	fn := c.getCopier(src.Type())
// 	return fn(src, visited, c)
// }

// func (c *Copier) getCopier(t reflect.Type) copierFn {
// 	if c.useCOW {
// 		return c.getCopierCOW(t)
// 	}
// 	return c.getCopierMutex(t)
// }

// // getCopierCOW: Copy-on-Write 策略，读路径无锁
// func (c *Copier) getCopierCOW(t reflect.Type) copierFn {
// 	// 快路径：原子读
// 	if m := c.cache.Load(); m != nil {
// 		if fn, ok := (*m)[t]; ok {
// 			return fn
// 		}
// 	}

// 	// 慢路径：加锁更新（复用 muCache 的写锁）
// 	c.muCache.Lock()

// 	// 双检
// 	if m := c.cache.Load(); m != nil {
// 		if fn, ok := (*m)[t]; ok {
// 			c.muCache.Unlock()
// 			return fn
// 		}
// 	}

// 	// 创建一个占位符 copier，先存入缓存，释放锁后再生成实际 copier
// 	// 这样可以避免在持有锁的情况下递归调用 generateCopier 导致死锁
// 	var placeholder copierFn
// 	placeholder = func(src reflect.Value, visited map[visitKey]reflect.Value, c *Copier) reflect.Value {
// 		// 占位符会在实际 copier 生成后被替换，所以这里直接调用 getCopier
// 		actualFn := c.getCopierCOW(t)
// 		return actualFn(src, visited, c)
// 	}

// 	// COW: 拷贝旧 map 并添加占位符
// 	oldPtr := c.cache.Load()
// 	oldLen := 0
// 	if oldPtr != nil {
// 		oldLen = len(*oldPtr)
// 	}
// 	newCache := make(copierCache, oldLen+1)
// 	if oldPtr != nil {
// 		for k, v := range *oldPtr {
// 			newCache[k] = v
// 		}
// 	}
// 	newCache[t] = placeholder
// 	c.cache.Store(&newCache)
// 	c.muCache.Unlock()

// 	// 在锁外生成实际 copier，避免死锁
// 	fn := c.generateCopier(t)

// 	// 再次加锁，将实际 copier 存入缓存
// 	c.muCache.Lock()
// 	oldPtr = c.cache.Load()
// 	newCache = make(copierCache, len(*oldPtr))
// 	for k, v := range *oldPtr {
// 		newCache[k] = v
// 	}
// 	newCache[t] = fn
// 	c.cache.Store(&newCache)
// 	c.muCache.Unlock()

// 	return fn
// }

// // getCopierMutex: 原地更新策略，避免 O(N^2) 预热
// func (c *Copier) getCopierMutex(t reflect.Type) copierFn {
// 	// 延迟初始化 map
// 	c.cacheInit.Do(func() {
// 		c.mapCache = make(copierCache)
// 	})

// 	// 快路径：RLock 读
// 	c.muCache.RLock()
// 	if fn, ok := c.mapCache[t]; ok {
// 		c.muCache.RUnlock()
// 		return fn
// 	}
// 	c.muCache.RUnlock()

// 	// 慢路径：Lock 写
// 	c.muCache.Lock()

// 	// 双检
// 	if fn, ok := c.mapCache[t]; ok {
// 		c.muCache.Unlock()
// 		return fn
// 	}

// 	// 创建占位符 copier，先存入缓存，释放锁后再生成实际 copier
// 	// 这样可以避免在持有锁的情况下递归调用 generateCopier 导致死锁
// 	var placeholder copierFn
// 	placeholder = func(src reflect.Value, visited map[visitKey]reflect.Value, c *Copier) reflect.Value {
// 		actualFn := c.getCopierMutex(t)
// 		return actualFn(src, visited, c)
// 	}
// 	c.mapCache[t] = placeholder
// 	c.muCache.Unlock()

// 	// 在锁外生成实际 copier，避免死锁
// 	fn := c.generateCopier(t)

// 	// 再次加锁，将实际 copier 存入缓存
// 	c.muCache.Lock()
// 	c.mapCache[t] = fn
// 	c.muCache.Unlock()

// 	return fn
// }

// func (c *Copier) generateCopier(t reflect.Type) copierFn {
// 	switch t.Kind() {
// 	case reflect.Ptr:
// 		return c.makePtrCopier(t)
// 	case reflect.Struct:
// 		return c.makeStructCopier(t)
// 	case reflect.Slice:
// 		return c.makeSliceCopier(t)
// 	case reflect.Array:
// 		return c.makeArrayCopier(t)
// 	case reflect.Map:
// 		return c.makeMapCopier(t)
// 	case reflect.Interface:
// 		return c.makeInterfaceCopier(t)
// 	case reflect.Chan, reflect.Func:
// 		// Channel 和 Func 不可拷贝，返回零值
// 		return func(src reflect.Value, visited map[visitKey]reflect.Value, c *Copier) reflect.Value {
// 			return reflect.Zero(t)
// 		}
// 	default:
// 		return func(src reflect.Value, visited map[visitKey]reflect.Value, c *Copier) reflect.Value {
// 			return src
// 		}
// 	}
// }

// func (c *Copier) makePtrCopier(t reflect.Type) copierFn {
// 	elemType := t.Elem()
// 	return func(src reflect.Value, visited map[visitKey]reflect.Value, c *Copier) reflect.Value {
// 		if src.IsNil() {
// 			return reflect.Zero(t)
// 		}
// 		ptr := src.Pointer()
// 		key := visitKey{ptr: ptr, typ: t}
// 		if c.handleCycle {
// 			if cached, ok := visited[key]; ok {
// 				return cached
// 			}
// 		}
// 		dst := reflect.New(elemType)
// 		if c.handleCycle {
// 			visited[key] = dst
// 		}
// 		elemFn := c.getCopier(elemType)
// 		copiedElem := elemFn(src.Elem(), visited, c)
// 		dst.Elem().Set(copiedElem)
// 		return dst
// 	}
// }

// func (c *Copier) makeStructCopier(t reflect.Type) copierFn {
// 	type fieldMeta struct {
// 		index     int
// 		offset    uintptr
// 		fieldType reflect.Type
// 		canSet    bool
// 		copier    copierFn // 预计算的字段 copier，避免运行时获取锁
// 	}

// 	fields := make([]fieldMeta, 0, t.NumField())
// 	for i := 0; i < t.NumField(); i++ {
// 		f := t.Field(i)
// 		// 预计算字段的 copier，避免在闭包中调用 c.getCopier 导致死锁
// 		fieldCopier := c.getCopier(f.Type)
// 		fields = append(fields, fieldMeta{
// 			index:     i,
// 			offset:    f.Offset,
// 			fieldType: f.Type,
// 			canSet:    f.PkgPath == "",
// 			copier:    fieldCopier,
// 		})
// 	}

// 	return func(src reflect.Value, visited map[visitKey]reflect.Value, c *Copier) reflect.Value {
// 		dst := reflect.New(t).Elem()

// 		// 检查 src 是否可寻址（用于 unexported 字段复制）
// 		srcCanAddr := src.CanAddr()

// 		for _, fm := range fields {
// 			if fm.canSet {
// 				srcField := src.Field(fm.index)
// 				copied := fm.copier(srcField, visited, c)
// 				dst.Field(fm.index).Set(copied)
// 			} else if c.copyUnexported && srcCanAddr {
// 				// 只有当 src 可寻址时才能复制未导出字段
// 				srcPtr := unsafe.Pointer(src.UnsafeAddr() + fm.offset)
// 				srcField := reflect.NewAt(fm.fieldType, srcPtr).Elem()

// 				copied := fm.copier(srcField, visited, c)

// 				// 使用 Set 来设置未导出字段（通过 unsafe）
// 				dstPtr := unsafe.Pointer(dst.UnsafeAddr() + fm.offset)
// 				// 对于不可寻址的 copied 值，我们需要创建一个可寻址的副本
// 				if copied.CanAddr() {
// 					runtimeMemmove(dstPtr, unsafe.Pointer(copied.UnsafeAddr()), fm.fieldType.Size())
// 				} else {
// 					// 如果 copied 不可寻址，使用反射设置
// 					dstField := reflect.NewAt(fm.fieldType, dstPtr).Elem()
// 					dstField.Set(copied)
// 				}
// 			}
// 		}
// 		return dst
// 	}
// }

// // makeSliceCopier 创建切片拷贝器，支持循环引用检测
// func (c *Copier) makeSliceCopier(t reflect.Type) copierFn {
// 	elemType := t.Elem()
// 	isPOD := isPlainOldData(elemType)
// 	// 预计算元素 copier，避免运行时获取锁
// 	elemFn := c.getCopier(elemType)

// 	return func(src reflect.Value, visited map[visitKey]reflect.Value, c *Copier) reflect.Value {
// 		if src.IsNil() {
// 			return reflect.Zero(t)
// 		}

// 		// 循环引用检测：检查切片是否已被拷贝
// 		if c.handleCycle {
// 			ptr := src.Pointer()
// 			key := visitKey{ptr: ptr, typ: t}
// 			if cached, ok := visited[key]; ok {
// 				return cached
// 			}
// 		}

// 		n := src.Len()
// 		dst := reflect.MakeSlice(t, n, src.Cap())

// 		// 在拷贝元素前将切片加入 visited，防止循环引用
// 		if c.handleCycle {
// 			visited[visitKey{ptr: src.Pointer(), typ: t}] = dst
// 		}

// 		if isPOD {
// 			reflect.Copy(dst, src)
// 			return dst
// 		}

// 		for i := 0; i < n; i++ {
// 			elem := src.Index(i)
// 			copied := elemFn(elem, visited, c)
// 			dst.Index(i).Set(copied)
// 		}
// 		return dst
// 	}
// }

// func (c *Copier) makeArrayCopier(t reflect.Type) copierFn {
// 	elemType := t.Elem()
// 	length := t.Len()
// 	isPOD := isPlainOldData(elemType)
// 	// 预计算元素 copier，避免运行时获取锁
// 	elemFn := c.getCopier(elemType)

// 	return func(src reflect.Value, visited map[visitKey]reflect.Value, c *Copier) reflect.Value {
// 		dst := reflect.New(t).Elem()
// 		if isPOD {
// 			// 对于 POD 类型，逐个元素复制（避免不可寻址问题）
// 			for i := 0; i < length; i++ {
// 				dst.Index(i).Set(src.Index(i))
// 			}
// 			return dst
// 		}
// 		for i := 0; i < length; i++ {
// 			copied := elemFn(src.Index(i), visited, c)
// 			dst.Index(i).Set(copied)
// 		}
// 		return dst
// 	}
// }

// func (c *Copier) makeMapCopier(t reflect.Type) copierFn {
// 	keyType := t.Key()
// 	elemType := t.Elem()
// 	// 预计算 key 和 value 的 copier，避免运行时获取锁
// 	keyFn := c.getCopier(keyType)
// 	elemFn := c.getCopier(elemType)

// 	return func(src reflect.Value, visited map[visitKey]reflect.Value, c *Copier) reflect.Value {
// 		if src.IsNil() {
// 			return reflect.Zero(t)
// 		}

// 		// 循环引用检测：检查 map 是否已被拷贝
// 		if c.handleCycle {
// 			ptr := src.Pointer()
// 			key := visitKey{ptr: ptr, typ: t}
// 			if cached, ok := visited[key]; ok {
// 				return cached
// 			}
// 		}

// 		dst := reflect.MakeMapWithSize(t, src.Len())

// 		// 在拷贝前将 map 加入 visited，防止循环引用
// 		if c.handleCycle {
// 			visited[visitKey{ptr: src.Pointer(), typ: t}] = dst
// 		}

// 		for _, key := range src.MapKeys() {
// 			// Key 也需要参与 visited 检测，防止 Key 包含指向 Map 的指针导致循环
// 			newKey := keyFn(key, visited, c)
// 			val := src.MapIndex(key)
// 			newVal := elemFn(val, visited, c)
// 			dst.SetMapIndex(newKey, newVal)
// 		}
// 		return dst
// 	}
// }

// func (c *Copier) makeInterfaceCopier(t reflect.Type) copierFn {
// 	return func(src reflect.Value, visited map[visitKey]reflect.Value, c *Copier) reflect.Value {
// 		if src.IsNil() {
// 			return reflect.Zero(t)
// 		}
// 		actual := src.Elem()
// 		// 深拷贝具体值，然后包装回接口类型
// 		copied := c.deepCopy(actual, visited)
// 		// 确保返回的是接口类型的 Value，而不是具体类型的 Value
// 		if copied.Type() != t {
// 			// 如果拷贝后的类型与接口类型不同，需要转换
// 			return copied.Convert(t)
// 		}
// 		return copied
// 	}
// }

// func isPlainOldData(t reflect.Type) bool {
// 	switch t.Kind() {
// 	case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
// 		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
// 		reflect.Float32, reflect.Float64, reflect.Complex64, reflect.Complex128:
// 		return true
// 	case reflect.Array:
// 		return isPlainOldData(t.Elem())
// 	default:
// 		return false
// 	}
// }

// // Copy 便捷函数（使用全局单例）
// func Copy(dst, src interface{}) error {
// 	globalCopierOnce.Do(func() {
// 		globalCopier = New()
// 	})
// 	return globalCopier.Copy(dst, src)
// }
