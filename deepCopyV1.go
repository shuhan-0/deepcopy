package deepcopy
// 最低适用于1.13版本
// import (
// 	"fmt"
// 	"reflect"
// 	"sync"
// 	"unsafe"
// )

// // visitKey 用于循环引用检测（仅用于指针、Map、Chan，明确排除 Slice）
// type visitKey struct {
// 	ptr uintptr
// 	typ reflect.Type
// }

// // copierEntry 包装 sync.Once 确保每个类型仅编译一次
// type copierEntry struct {
// 	once sync.Once
// 	fn   copierFn
// }

// // copierFn 是针对特定类型的无反射拷贝函数
// type copierFn func(src reflect.Value, visited map[visitKey]reflect.Value, c *Copier) reflect.Value

// // Copier 提供深拷贝能力
// type Copier struct {
// 	cache          sync.Map // map[reflect.Type]*copierEntry
// 	handleCycle    bool     // 是否处理循环引用（默认 true）
// 	copyUnexported bool     // 是否拷贝未导出字段（默认 false）
// }

// // New 创建 Copier 实例
// func New() *Copier {
// 	return &Copier{
// 		handleCycle:    true,
// 		copyUnexported: false,
// 	}
// }

// // SetCopyUnexported 开启未导出字段拷贝
// func (c *Copier) SetCopyUnexported(enable bool) *Copier {
// 	c.copyUnexported = enable
// 	return c
// }

// // SetHandleCycle 配置循环引用检测
// func (c *Copier) SetHandleCycle(enable bool) *Copier {
// 	c.handleCycle = enable
// 	return c
// }

// // Copy 执行深拷贝，语义：Copy(dst, src)，dst 必须为非 nil 指针
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

// // Clone 便捷函数，返回 src 的深拷贝
// func Clone(src interface{}) (interface{}, error) {
// 	if src == nil {
// 		return nil, nil
// 	}

// 	c := New()
// 	srcVal := reflect.ValueOf(src)
// 	visited := make(map[visitKey]reflect.Value)
// 	result := c.deepCopy(srcVal, visited)
// 	return result.Interface(), nil
// }

// func (c *Copier) deepCopy(src reflect.Value, visited map[visitKey]reflect.Value) reflect.Value {
// 	copier := c.getCopier(src.Type())
// 	return copier(src, visited, c)
// }

// func (c *Copier) getCopier(t reflect.Type) copierFn {
// 	v, _ := c.cache.LoadOrStore(t, &copierEntry{})
// 	entry := v.(*copierEntry)
// 	entry.once.Do(func() {
// 		entry.fn = c.generateCopier(t)
// 	})
// 	return entry.fn
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

// 		elemCopier := c.getCopier(elemType)
// 		copiedElem := elemCopier(src.Elem(), visited, c)
// 		dst.Elem().Set(copiedElem)

// 		return dst
// 	}
// }

// func (c *Copier) makeStructCopier(t reflect.Type) copierFn {
// 	type fieldMeta struct {
// 		index     int
// 		offset    uintptr
// 		copier    copierFn
// 		canSet    bool
// 		fieldType reflect.Type
// 		isPointer bool // 标记是否为指针类型，用于优化
// 	}

// 	fields := make([]fieldMeta, 0, t.NumField())
// 	for i := 0; i < t.NumField(); i++ {
// 		f := t.Field(i)
// 		fields = append(fields, fieldMeta{
// 			index:     i,
// 			offset:    f.Offset,
// 			copier:    c.getCopier(f.Type),
// 			canSet:    f.PkgPath == "",
// 			fieldType: f.Type,
// 			isPointer: f.Type.Kind() == reflect.Ptr,
// 		})
// 	}

// 	return func(src reflect.Value, visited map[visitKey]reflect.Value, c *Copier) reflect.Value {
// 		dst := reflect.New(t).Elem()

// 		for _, fm := range fields {
// 			if fm.canSet {
// 				srcField := src.Field(fm.index)
// 				copied := fm.copier(srcField, visited, c)
// 				dst.Field(fm.index).Set(copied)
// 			} else if c.copyUnexported {
// 				// 处理未导出字段：通过 unsafe 地址构造 Value，深拷贝后整体搬运
// 				srcPtr := unsafe.Pointer(src.UnsafeAddr() + fm.offset)
// 				srcField := reflect.NewAt(fm.fieldType, srcPtr).Elem()

// 				// 递归深拷贝（这里 copied 已经是全新的、独立的对象）
// 				copied := fm.copier(srcField, visited, c)

// 				// 将 copied 的内容整体搬运到目标字段
// 				dstPtr := unsafe.Pointer(dst.UnsafeAddr() + fm.offset)
// 				c.memmove(dstPtr, unsafe.Pointer(copied.UnsafeAddr()), fm.fieldType.Size())
// 			}
// 		}
// 		return dst
// 	}
// }

// // makeSliceCopier 处理切片
// func (c *Copier) makeSliceCopier(t reflect.Type) copierFn {
// 	elemType := t.Elem()
// 	elemCopier := c.getCopier(elemType)
// 	isBasic := isPlainOldData(elemType)

// 	return func(src reflect.Value, visited map[visitKey]reflect.Value, c *Copier) reflect.Value {
// 		if src.IsNil() {
// 			return reflect.Zero(t)
// 		}

// 		n := src.Len()
// 		dst := reflect.MakeSlice(t, n, src.Cap())

// 		if isBasic {
// 			// POD 类型：使用 reflect.Copy（编译器优化为 memmove）
// 			reflect.Copy(dst, src)
// 			return dst
// 		}

// 		// 非 POD 类型：逐元素递归深拷贝
// 		// 注意：不检查 visited，每个切片元素都独立拷贝
// 		// 如果元素包含指针，在元素级别的 copier 中处理循环引用
// 		for i := 0; i < n; i++ {
// 			elem := src.Index(i)
// 			copied := elemCopier(elem, visited, c)
// 			dst.Index(i).Set(copied)
// 		}
// 		return dst
// 	}
// }

// func (c *Copier) makeArrayCopier(t reflect.Type) copierFn {
// 	elemType := t.Elem()
// 	length := t.Len()
// 	elemCopier := c.getCopier(elemType)
// 	isBasic := isPlainOldData(elemType)

// 	return func(src reflect.Value, visited map[visitKey]reflect.Value, c *Copier) reflect.Value {
// 		dst := reflect.New(t).Elem()

// 		if isBasic {
// 			reflect.Copy(dst.Slice(0, length), src.Slice(0, length))
// 			return dst
// 		}

// 		for i := 0; i < length; i++ {
// 			copied := elemCopier(src.Index(i), visited, c)
// 			dst.Index(i).Set(copied)
// 		}
// 		return dst
// 	}
// }

// func (c *Copier) makeMapCopier(t reflect.Type) copierFn {
// 	keyCopier := c.getCopier(t.Key())
// 	elemCopier := c.getCopier(t.Elem())

// 	return func(src reflect.Value, visited map[visitKey]reflect.Value, c *Copier) reflect.Value {
// 		if src.IsNil() {
// 			return reflect.Zero(t)
// 		}

// 		if c.handleCycle {
// 			ptr := src.Pointer()
// 			key := visitKey{ptr: ptr, typ: t}
// 			if cached, ok := visited[key]; ok {
// 				return cached
// 			}
// 		}

// 		dst := reflect.MakeMapWithSize(t, src.Len())

// 		if c.handleCycle {
// 			visited[visitKey{ptr: src.Pointer(), typ: t}] = dst
// 		}

// 		for _, key := range src.MapKeys() {
// 			newKey := keyCopier(key, visited, c)
// 			val := src.MapIndex(key)
// 			newVal := elemCopier(val, visited, c)
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
// 		copied := c.deepCopy(actual, visited)
// 		return copied
// 	}
// }

// // isPlainOldData 判断是否为纯值类型（不包含指针，GC 不扫描）
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

// // memmove 整块内存拷贝，用于未导出字段的写入
// // 关键：此时 src 已经是深拷贝后的全新对象，整块搬运不会引入浅拷贝问题
// // 使用分块策略避免栈溢出
// func (c *Copier) memmove(dst, src unsafe.Pointer, n uintptr) {
// 	if n == 0 || dst == src {
// 		return
// 	}

// 	// 使用中等大小的数组，平衡性能和栈安全
// 	const blockSize = 64 * 1024

// 	for n > 0 {
// 		size := n
// 		if size > blockSize {
// 			size = blockSize
// 		}

// 		// 转换为适当大小的数组指针进行拷贝
// 		// 注意：这里使用的是 copied 对象（src）的内存，不是原对象
// 		srcSlice := (*[blockSize]byte)(src)[:size:size]
// 		dstSlice := (*[blockSize]byte)(dst)[:size:size]
// 		copy(dstSlice, srcSlice)

// 		src = unsafe.Pointer(uintptr(src) + size)
// 		dst = unsafe.Pointer(uintptr(dst) + size)
// 		n -= size
// 	}
// }

// // Copy 便捷函数
// func Copy(dst, src interface{}) error {
// 	return New().Copy(dst, src)
// }