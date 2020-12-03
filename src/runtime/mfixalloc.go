// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Fixed-size object allocator. Returned memory is not zeroed.
//
// See malloc.go for overview.

package runtime

import "unsafe"

// FixAlloc is a simple free-list allocator for fixed size objects.
// Malloc uses a FixAlloc wrapped around sysAlloc to manage its
// mcache and mspan objects.
//
// Memory returned by fixalloc.alloc is zeroed by default, but the
// caller may take responsibility for zeroing allocations by setting
// the zero flag to false. This is only safe if the memory never
// contains heap pointers.
//
// The caller is responsible for locking around FixAlloc calls.
// Callers can keep state in the object but the first word is
// smashed by freeing and reallocating.
//
// Consider marking fixalloc'd types go:notinheap.
// fixalloc 是一个简单的固定大小对象的自由表内存分配器。
// Malloc 使用围绕 sysAlloc 的 fixalloc 来管理其 MCache 和 MSpan 对象。
//
// fixalloc.alloc 返回的内存默认为零，但调用者可以通过将 zero 标志设置为 false
// 来自行负责将分配归零。如果这部分内存永远不包含堆指针，则这样的操作是安全的。
//
// 调用方负责锁定 fixalloc 调用。调用方可以在对象中保持状态，
// 但当释放和重新分配时第一个字会被破坏。

// fixalloc 是一个基于自由列表的固定大小的分配器。其核心原理是将若干未分配的内存块连接起来， 将未分配的区域的第一个字为指向下一个未分配区域的指针使用。
//
// Go 的主分配堆中 malloc（span、cache、treap、finalizer、profile、arena hint 等） 均 围绕它为实体进行固定分配和回收。
// fixalloc获取的内存不由golang的内存模型管理
type fixalloc struct {
	// 分配的内存大小
	size uintptr
	// todo 首次调用时返回 p
	first func(arg, p unsafe.Pointer) // called first time p is returned
	// first函数的arg参数
	arg unsafe.Pointer
	// 当调用了free的时候，会将free的obj挂到list上，如果下次再申请，可以进行复用
	list *mlink

	// 内存块的首地址，当再次alloc的时候，如果list中没有，则从该地址开始分配
	chunk uintptr // use uintptr instead of unsafe.Pointer to avoid write barriers
	// 剩余带分配的地址，如果不足以满足一个size，则重新向操作系统申请
	nchunk uint32
	// 正在使用的字节
	inuse uintptr // in-use bytes now
	stat  *uint64
	// 分配时，是否对内存进行清0
	zero bool // zero allocations
}

// A generic linked list of blocks.  (Typically the block is bigger than sizeof(MLink).)
// Since assignments to mlink.next will result in a write barrier being performed
// this cannot be used by some of the internal GC structures. For example when
// the sweeper is placing an unmarked object on the free list it does not want the
// write barrier to be called since that could result in the object being reachable.
//
//go:notinheap
type mlink struct {
	next *mlink
}

// Initialize f to allocate objects of the given size,
// using the allocator to obtain chunks of memory.
// 初始化
func (f *fixalloc) init(size uintptr, first func(arg, p unsafe.Pointer), arg unsafe.Pointer, stat *uint64) {
	f.size = size   // 设置结构分配的大小
	f.first = first // 设置函数
	f.arg = arg     // 设置参数
	f.list = nil
	f.chunk = 0
	f.nchunk = 0
	f.inuse = 0
	f.stat = stat
	f.zero = true
}

func (f *fixalloc) alloc() unsafe.Pointer {
	if f.size == 0 {
		print("runtime: use of FixAlloc_Alloc before FixAlloc_Init\n")
		throw("runtime: internal error")
	}

	// 如果list不为空，则有之前释放的，从释放队列返回一个回去，复用之前释放的空间
	if f.list != nil {
		v := unsafe.Pointer(f.list)
		f.list = f.list.next
		f.inuse += f.size
		// 如果需要清0， 对空间的内存进行清0
		if f.zero {
			memclrNoHeapPointers(v, f.size)
		}
		return v
	}

	// 如果此时 nchunk 不足以分配一个 size
	if uintptr(f.nchunk) < f.size {
		// 则向操作系统申请内存，大小为 16k
		f.chunk = uintptr(persistentalloc(_FixAllocChunk, 0, f.stat))
		f.nchunk = _FixAllocChunk
	}

	// 获取v
	v := unsafe.Pointer(f.chunk)
	if f.first != nil {
		// 调用第一次执行函数来执行
		f.first(f.arg, v)
	}
	// 已经分配了一个obj，chunk往后移下，给下个元素
	f.chunk = f.chunk + f.size
	// 剩余分配的元素减少
	f.nchunk -= uint32(f.size)
	// 使用中的内存
	f.inuse += f.size
	return v
}

// 释放一个p
func (f *fixalloc) free(p unsafe.Pointer) {
	// 减去使用的大小，并将p挂到f.list的头部
	f.inuse -= f.size
	v := (*mlink)(p)
	v.next = f.list
	f.list = v
}
