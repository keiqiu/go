// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sync

import (
	"internal/race"
	"runtime"
	"sync/atomic"
	"unsafe"
)

// A Pool is a set of temporary objects that may be individually saved and
// retrieved.
//
// Any item stored in the Pool may be removed automatically at any time without
// notification. If the Pool holds the only reference when this happens, the
// item might be deallocated.
//
// A Pool is safe for use by multiple goroutines simultaneously.
//
// Pool's purpose is to cache allocated but unused items for later reuse,
// relieving pressure on the garbage collector. That is, it makes it easy to
// build efficient, thread-safe free lists. However, it is not suitable for all
// free lists.
//
// An appropriate use of a Pool is to manage a group of temporary items
// silently shared among and potentially reused by concurrent independent
// clients of a package. Pool provides a way to amortize allocation overhead
// across many clients.
//
// An example of good use of a Pool is in the fmt package, which maintains a
// dynamically-sized store of temporary output buffers. The store scales under
// load (when many goroutines are actively printing) and shrinks when
// quiescent.
//
// On the other hand, a free list maintained as part of a short-lived object is
// not a suitable use for a Pool, since the overhead does not amortize well in
// that scenario. It is more efficient to have such objects implement their own
// free list.
//
// A Pool must not be copied after first use.
type Pool struct {
	noCopy noCopy // 禁止拷贝的标记
	// 一个指针，直降不同的poolLocal
	local     unsafe.Pointer // local fixed-size per-P pool, actual type is [P]poolLocal
	localSize uintptr        // size of the local array

	// 保存旧的local
	victim unsafe.Pointer // local from previous cycle
	// 保存旧的localsize
	victimSize uintptr // size of victims array

	// New optionally specifies a function to generate
	// a value when Get would otherwise return nil.
	// It may not be changed concurrently with calls to Get.
	New func() interface{}
}

// Local per-P Pool appendix.
type poolLocalInternal struct {
	// 保存的一个p对象， 方便快速获取 取值的时候先取private，没有再去shared取
	private interface{} // Can be used only by the respective P.
	// 本地p的迟滞，只有p可poptail
	shared poolChain // Local P can pushHead/popHead; any P can popTail.
}

type poolLocal struct {
	poolLocalInternal

	// Prevents false sharing on widespread platforms with
	// 128 mod (cache line size) = 0 .
	// 填充字段，没明白为什么128位
	pad [128 - unsafe.Sizeof(poolLocalInternal{})%128]byte
}

// from runtime
func fastrand() uint32

var poolRaceHash [128]uint64

// poolRaceAddr returns an address to use as the synchronization point
// for race detector logic. We don't use the actual pointer stored in x
// directly, for fear of conflicting with other synchronization on that address.
// Instead, we hash the pointer to get an index into poolRaceHash.
// See discussion on golang.org/cl/31589.
func poolRaceAddr(x interface{}) unsafe.Pointer {
	ptr := uintptr((*[2]unsafe.Pointer)(unsafe.Pointer(&x))[1])
	h := uint32((uint64(uint32(ptr)) * 0x85ebca6b) >> 16)
	return unsafe.Pointer(&poolRaceHash[h%uint32(len(poolRaceHash))])
}

// Put adds x to the pool.
// 将元素规划只pool中
func (p *Pool) Put(x interface{}) {
	if x == nil {
		return
	}
	if race.Enabled {
		if fastrand()%4 == 0 {
			// Randomly drop x on floor.
			return
		}
		race.ReleaseMerge(poolRaceAddr(x))
		race.Disable()
	}
	l, _ := p.pin()

	// 如果private为nil，则先归还至private
	if l.private == nil {
		l.private = x
		x = nil
	}

	// 如果没归还到private，就push到shared的头部
	// 操作当前p都在头部，操作其他的p都在尾部，这样就无需锁
	if x != nil {
		l.shared.pushHead(x)
	}
	runtime_procUnpin()
	if race.Enabled {
		race.Enable()
	}
}

// Get selects an arbitrary item from the Pool, removes it from the
// Pool, and returns it to the caller.
// Get may choose to ignore the pool and treat it as empty.
// Callers should not assume any relation between values passed to Put and
// the values returned by Get.
//
// If Get would otherwise return nil and p.New is non-nil, Get returns
// the result of calling p.New.
// 从pool中获取一个对象
func (p *Pool) Get() interface{} {
	if race.Enabled {
		race.Disable()
	}
	// 获取当前协程所对应的poolLocal
	l, pid := p.pin()
	x := l.private
	l.private = nil
	// 如果x是nil, 则尝试从shared中取，如果还没有 就去其他的p中偷
	if x == nil {
		// Try to pop the head of the local shard. We prefer
		// the head over the tail for temporal locality of
		// reuse.
		x, _ = l.shared.popHead()
		if x == nil {
			x = p.getSlow(pid)
		}
	}
	// 解锁，允许gc
	runtime_procUnpin()
	if race.Enabled {
		race.Enable()
		if x != nil {
			race.Acquire(poolRaceAddr(x))
		}
	}

	// 如果还没有，并且new函数存在，则初始化一个
	if x == nil && p.New != nil {
		x = p.New()
	}
	return x
}

// 尝试从其他p的local中找，如果没有继续从旧的local（victim）中找
func (p *Pool) getSlow(pid int) interface{} {
	// See the comment in pin regarding ordering of the loads.
	size := atomic.LoadUintptr(&p.localSize) // load-acquire
	locals := p.local                        // load-consume
	// Try to steal one element from other procs.
	// 遍历所有的locals，从他的尾部偷元素，如果偷到了 就会犯
	for i := 0; i < int(size); i++ {
		l := indexLocal(locals, (pid+i+1)%int(size))
		if x, _ := l.shared.popTail(); x != nil {
			return x
		}
	}

	// Try the victim cache. We do this after attempting to steal
	// from all primary caches because we want objects in the
	// victim cache to age out if at all possible.
	// 如果所有的p都没有元素了
	size = atomic.LoadUintptr(&p.victimSize)
	// 如果pid大于size， 那么victim中肯定没有
	if uintptr(pid) >= size {
		return nil
	}
	locals = p.victim
	l := indexLocal(locals, pid)
	// 遍历旧的private， 如果有就找到了就返回
	if x := l.private; x != nil {
		l.private = nil
		return x
	}
	// 从旧的所有local的尾部开始偷
	for i := 0; i < int(size); i++ {
		l := indexLocal(locals, (pid+i)%int(size))
		if x, _ := l.shared.popTail(); x != nil {
			return x
		}
	}

	// Mark the victim cache as empty for future gets don't bother
	// with it.
	// 如果旧的里面都没有，以后不用从victim里找了，将victimSize标记为空
	atomic.StoreUintptr(&p.victimSize, 0)

	return nil
}

// pin pins the current goroutine to P, disables preemption and
// returns poolLocal pool for the P and the P's id.
// Caller must call runtime_procUnpin() when done with the pool.
// 获取当前协程所对应的poolLocal 和 proc_id
func (p *Pool) pin() (*poolLocal, int) {
	// 获取当前GPM中P的id
	pid := runtime_procPin()
	// In pinSlow we store to local and then to localSize, here we load in opposite order.
	// Since we've disabled preemption, GC cannot happen in between.
	// Thus here we must observe local at least as large localSize.
	// We can observe a newer/larger local, it is fine (we must observe its zero-initialized-ness).
	// 获取当前p.local的size
	s := atomic.LoadUintptr(&p.localSize) // load-acquire
	l := p.local                          // load-consume
	// 如果pid < s 则说明proc的池子已经创建了，直接返回
	if uintptr(pid) < s {
		return indexLocal(l, pid), pid
	}
	// 初始化新p
	return p.pinSlow()
}

func (p *Pool) pinSlow() (*poolLocal, int) {
	// Retry under the mutex.
	// Can not lock the mutex while pinned.
	// 解锁p，下面要重新锁pool后再锁pin
	runtime_procUnpin()
	// 锁掉pool
	allPoolsMu.Lock()
	defer allPoolsMu.Unlock()
	// 加锁proc
	pid := runtime_procPin()
	// poolCleanup won't be called while we are pinned.
	s := p.localSize
	l := p.local
	// 再次判断，如果有了，就返回
	if uintptr(pid) < s {
		return indexLocal(l, pid), pid
	}
	// 如果p.local为空，第一个请求，将这个pool对象添加到allPools中
	if p.local == nil {
		allPools = append(allPools, p)
	}
	// If GOMAXPROCS changes between GCs, we re-allocate the array and lose the old one.
	// 获取proc的数量
	size := runtime.GOMAXPROCS(0)
	// 初始化local
	local := make([]poolLocal, size)
	// 将local切片的头指针赋值给p.local， 将size赋值个p.localSize
	// 此处需要用原子操作，突破可见性，不然可能出现proc1 设置了local和localSize， 而proc2没有读到
	atomic.StorePointer(&p.local, unsafe.Pointer(&local[0])) // store-release
	atomic.StoreUintptr(&p.localSize, uintptr(size))         // store-release
	return &local[pid], pid
}

// 清理pool
// 函数运行时处于gc清理的操作，此时处于stw状态，没有并发和安全的问题
func poolCleanup() {
	// This function is called with the world stopped, at the beginning of a garbage collection.
	// It must not allocate and probably should not call any runtime functions.

	// Because the world is stopped, no pool user can be in a
	// pinned section (in effect, this has all Ps pinned).

	// Drop victim caches from all pools.
	// 清理旧pools的所有对象
	for _, p := range oldPools {
		p.victim = nil
		p.victimSize = 0
	}

	// Move primary cache to victim cache.
	// 将所有p的local移到victim，p.local归0
	for _, p := range allPools {
		p.victim = p.local
		p.victimSize = p.localSize
		p.local = nil
		p.localSize = 0
	}

	// The pools with non-empty primary caches now have non-empty
	// victim caches and no pools have primary caches.
	// 销毁上一次gc留下的oldPools
	oldPools, allPools = allPools, nil
}

var (
	// 锁
	allPoolsMu Mutex

	// allPools is the set of pools that have non-empty primary
	// caches. Protected by either 1) allPoolsMu and pinning or 2)
	// STW.
	// allPools是所有pool的集合， allPoolsMu加锁或者stw模式下是安全的
	allPools []*Pool

	// oldPools is the set of pools that may have non-empty victim
	// caches. Protected by STW.
	oldPools []*Pool
)

func init() {
	runtime_registerPoolCleanup(poolCleanup)
}

// 取出对应的p的p.local
func indexLocal(l unsafe.Pointer, i int) *poolLocal {
	lp := unsafe.Pointer(uintptr(l) + uintptr(i)*unsafe.Sizeof(poolLocal{}))
	return (*poolLocal)(lp)
}

// Implemented in runtime.
// 设置设置pool的函数，gc start的时候会执行
func runtime_registerPoolCleanup(cleanup func())

// 获取当前的pid，并且阻止gc
func runtime_procPin() int
func runtime_procUnpin()
