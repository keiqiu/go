// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Memory allocator.
//
// This was originally based on tcmalloc, but has diverged quite a bit.
// http://goog-perftools.sourceforge.net/doc/tcmalloc.html

// The main allocator works in runs of pages.
// Small allocation sizes (up to and including 32 kB) are
// rounded to one of about 70 size classes, each of which
// has its own free set of objects of exactly that size.
// Any free page of memory can be split into a set of objects
// of one size class, which are then managed using a free bitmap.
//
// The allocator's data structures are:
//
//	fixalloc: a free-list allocator for fixed-size off-heap objects,
//		used to manage storage used by the allocator.
//	mheap: the malloc heap, managed at page (8192-byte) granularity.
//	mspan: a run of pages managed by the mheap.
//	mcentral: collects all spans of a given size class.
//	mcache: a per-P cache of mspans with free space.
//	mstats: allocation statistics.
//
// Allocating a small object proceeds up a hierarchy of caches:
//
//	1. Round the size up to one of the small size classes
//	   and look in the corresponding mspan in this P's mcache.
//	   Scan the mspan's free bitmap to find a free slot.
//	   If there is a free slot, allocate it.
//	   This can all be done without acquiring a lock.
//
//	2. If the mspan has no free slots, obtain a new mspan
//	   from the mcentral's list of mspans of the required size
//	   class that have free space.
//	   Obtaining a whole span amortizes the cost of locking
//	   the mcentral.
//
//	3. If the mcentral's mspan list is empty, obtain a run
//	   of pages from the mheap to use for the mspan.
//
//	4. If the mheap is empty or has no page runs large enough,
//	   allocate a new group of pages (at least 1MB) from the
//	   operating system. Allocating a large run of pages
//	   amortizes the cost of talking to the operating system.
//
// Sweeping an mspan and freeing objects on it proceeds up a similar
// hierarchy:
//
//	1. If the mspan is being swept in response to allocation, it
//	   is returned to the mcache to satisfy the allocation.
//
//	2. Otherwise, if the mspan still has allocated objects in it,
//	   it is placed on the mcentral free list for the mspan's size
//	   class.
//
//	3. Otherwise, if all objects in the mspan are free, the mspan
//	   is now "idle", so it is returned to the mheap and no longer
//	   has a size class.
//	   This may coalesce it with adjacent idle mspans.
//
//	4. If an mspan remains idle for long enough, return its pages
//	   to the operating system.
//
// Allocating and freeing a large object uses the mheap
// directly, bypassing the mcache and mcentral.
//
// Free object slots in an mspan are zeroed only if mspan.needzero is
// false. If needzero is true, objects are zeroed as they are
// allocated. There are various benefits to delaying zeroing this way:
//
//	1. Stack frame allocation can avoid zeroing altogether.
//
//	2. It exhibits better temporal locality, since the program is
//	   probably about to write to the memory.
//
//	3. We don't zero pages that never get reused.

// Virtual memory layout
//
// The heap consists of a set of arenas, which are 64MB on 64-bit and
// 4MB on 32-bit (heapArenaBytes). Each arena's start address is also
// aligned to the arena size.
//
// Each arena has an associated heapArena object that stores the
// metadata for that arena: the heap bitmap for all words in the arena
// and the span map for all pages in the arena. heapArena objects are
// themselves allocated off-heap.
//
// Since arenas are aligned, the address space can be viewed as a
// series of arena frames. The arena map (mheap_.arenas) maps from
// arena frame number to *heapArena, or nil for parts of the address
// space not backed by the Go heap. The arena map is structured as a
// two-level array consisting of a "L1" arena map and many "L2" arena
// maps; however, since arenas are large, on many architectures, the
// arena map consists of a single, large L2 map.
//
// The arena map covers the entire possible address space, allowing
// the Go heap to use any part of the address space. The allocator
// attempts to keep arenas contiguous so that large spans (and hence
// large objects) can cross arenas.

package runtime

import (
	"runtime/internal/atomic"
	"runtime/internal/math"
	"runtime/internal/sys"
	"unsafe"
)

const (
	debugMalloc = false

	maxTinySize   = _TinySize
	tinySizeClass = _TinySizeClass
	maxSmallSize  = _MaxSmallSize

	pageShift = _PageShift
	pageSize  = _PageSize // 8k
	pageMask  = _PageMask
	// By construction, single page spans of the smallest object class
	// have the most objects per span.
	maxObjsPerSpan = pageSize / 8

	concurrentSweep = _ConcurrentSweep

	_PageSize = 1 << _PageShift
	_PageMask = _PageSize - 1

	// _64bit = 1 on 64-bit systems, 0 on 32-bit systems
	_64bit = 1 << (^uintptr(0) >> 63) / 2

	// Tiny allocator parameters, see "Tiny allocator" comment in malloc.go.
	_TinySize      = 16
	_TinySizeClass = int8(2)

	_FixAllocChunk = 16 << 10 // Chunk size for FixAlloc

	// Per-P, per order stack segment cache size.
	_StackCacheSize = 32 * 1024

	// Number of orders that get caching. Order 0 is FixedStack
	// and each successive order is twice as large.
	// We want to cache 2KB, 4KB, 8KB, and 16KB stacks. Larger stacks
	// will be allocated directly.
	// Since FixedStack is different on different systems, we
	// must vary NumStackOrders to keep the same maximum cached size.
	//   OS               | FixedStack | NumStackOrders
	//   -----------------+------------+---------------
	//   linux/darwin/bsd | 2KB        | 4
	//   windows/32       | 4KB        | 3
	//   windows/64       | 8KB        | 2
	//   plan9            | 4KB        | 3
	_NumStackOrders = 4 - sys.PtrSize/4*sys.GoosWindows - 1*sys.GoosPlan9

	// heapAddrBits is the number of bits in a heap address. On
	// amd64, addresses are sign-extended beyond heapAddrBits. On
	// other arches, they are zero-extended.
	//
	// On most 64-bit platforms, we limit this to 48 bits based on a
	// combination of hardware and OS limitations.
	//
	// amd64 hardware limits addresses to 48 bits, sign-extended
	// to 64 bits. Addresses where the top 16 bits are not either
	// all 0 or all 1 are "non-canonical" and invalid. Because of
	// these "negative" addresses, we offset addresses by 1<<47
	// (arenaBaseOffset) on amd64 before computing indexes into
	// the heap arenas index. In 2017, amd64 hardware added
	// support for 57 bit addresses; however, currently only Linux
	// supports this extension and the kernel will never choose an
	// address above 1<<47 unless mmap is called with a hint
	// address above 1<<47 (which we never do).
	//
	// arm64 hardware (as of ARMv8) limits user addresses to 48
	// bits, in the range [0, 1<<48).
	//
	// ppc64, mips64, and s390x support arbitrary 64 bit addresses
	// in hardware. On Linux, Go leans on stricter OS limits. Based
	// on Linux's processor.h, the user address space is limited as
	// follows on 64-bit architectures:
	//
	// Architecture  Name              Maximum Value (exclusive)
	// ---------------------------------------------------------------------
	// amd64         TASK_SIZE_MAX     0x007ffffffff000 (47 bit addresses)
	// arm64         TASK_SIZE_64      0x01000000000000 (48 bit addresses)
	// ppc64{,le}    TASK_SIZE_USER64  0x00400000000000 (46 bit addresses)
	// mips64{,le}   TASK_SIZE64       0x00010000000000 (40 bit addresses)
	// s390x         TASK_SIZE         1<<64 (64 bit addresses)
	//
	// These limits may increase over time, but are currently at
	// most 48 bits except on s390x. On all architectures, Linux
	// starts placing mmap'd regions at addresses that are
	// significantly below 48 bits, so even if it's possible to
	// exceed Go's 48 bit limit, it's extremely unlikely in
	// practice.
	//
	// On aix/ppc64, the limits is increased to 1<<60 to accept addresses
	// returned by mmap syscall. These are in range:
	//  0x0a00000000000000 - 0x0afffffffffffff
	//
	// On 32-bit platforms, we accept the full 32-bit address
	// space because doing so is cheap.
	// mips32 only has access to the low 2GB of virtual memory, so
	// we further limit it to 31 bits.
	//
	// WebAssembly currently has a limit of 4GB linear memory.
	heapAddrBits = (_64bit*(1-sys.GoarchWasm)*(1-sys.GoosAix))*48 + (1-_64bit+sys.GoarchWasm)*(32-(sys.GoarchMips+sys.GoarchMipsle)) + 60*sys.GoosAix

	// maxAlloc is the maximum size of an allocation. On 64-bit,
	// it's theoretically possible to allocate 1<<heapAddrBits bytes. On
	// 32-bit, however, this is one less than 1<<32 because the
	// number of bytes in the address space doesn't actually fit
	// in a uintptr.
	maxAlloc = (1 << heapAddrBits) - (1-_64bit)*1

	// The number of bits in a heap address, the size of heap
	// arenas, and the L1 and L2 arena map sizes are related by
	//
	//   (1 << addr bits) = arena size * L1 entries * L2 entries
	//
	// Currently, we balance these as follows:
	//
	//       Platform  Addr bits  Arena size  L1 entries   L2 entries
	// --------------  ---------  ----------  ----------  -----------
	//       */64-bit         48        64MB           1    4M (32MB)
	//     aix/64-bit         60       256MB        4096    4M (32MB)
	// windows/64-bit         48         4MB          64    1M  (8MB)
	//       */32-bit         32         4MB           1  1024  (4KB)
	//     */mips(le)         31         4MB           1   512  (2KB)

	// heapArenaBytes is the size of a heap arena. The heap
	// consists of mappings of size heapArenaBytes, aligned to
	// heapArenaBytes. The initial heap mapping is one arena.
	//
	// This is currently 64MB on 64-bit non-Windows and 4MB on
	// 32-bit and on Windows. We use smaller arenas on Windows
	// because all committed memory is charged to the process,
	// even if it's not touched. Hence, for processes with small
	// heaps, the mapped arena space needs to be commensurate.
	// This is particularly important with the race detector,
	// since it significantly amplifies the cost of committed
	// memory.
	// 当前版本的AMD64架构就规定了只用48位地址；一个表示虚拟内存地址的64位指针只有低48位有效并带符号扩展到64位——换句话说，其高16位必须是全1或全0，而且必须与低48位的最高位（第47位）一致，否则通过该地址访问内存会产生#GP异常（general-protection exception）。

	heapArenaBytes = 1 << logHeapArenaBytes // 64M

	// logHeapArenaBytes is log_2 of heapArenaBytes. For clarity,
	// prefer using heapArenaBytes where possible (we need the
	// constant to compute some other constants).
	logHeapArenaBytes = (6+20)*(_64bit*(1-sys.GoosWindows)*(1-sys.GoosAix)*(1-sys.GoarchWasm)) + (2+20)*(_64bit*sys.GoosWindows) + (2+20)*(1-_64bit) + (8+20)*sys.GoosAix + (2+20)*sys.GoarchWasm

	// heapArenaBitmapBytes is the size of each heap arena's bitmap.
	heapArenaBitmapBytes = heapArenaBytes / (sys.PtrSize * 8 / 2) // 2097152=2M

	pagesPerArena = heapArenaBytes / pageSize // 8192

	// arenaL1Bits is the number of bits of the arena number
	// covered by the first level arena map.
	//
	// This number should be small, since the first level arena
	// map requires PtrSize*(1<<arenaL1Bits) of space in the
	// binary's BSS. It can be zero, in which case the first level
	// index is effectively unused. There is a performance benefit
	// to this, since the generated code can be more efficient,
	// but comes at the cost of having a large L2 mapping.
	//
	// We use the L1 map on 64-bit Windows because the arena size
	// is small, but the address space is still 48 bits, and
	// there's a high cost to having a large L2.
	//
	// We use the L1 map on aix/ppc64 to keep the same L2 value
	// as on Linux.
	arenaL1Bits = 6*(_64bit*sys.GoosWindows) + 12*sys.GoosAix

	// arenaL2Bits is the number of bits of the arena number
	// covered by the second level arena index.
	//
	// The size of each arena map allocation is proportional to
	// 1<<arenaL2Bits, so it's important that this not be too
	// large. 48 bits leads to 32MB arena index allocations, which
	// is about the practical threshold.
	arenaL2Bits = heapAddrBits - logHeapArenaBytes - arenaL1Bits

	// arenaL1Shift is the number of bits to shift an arena frame
	// number by to compute an index into the first level arena map.
	arenaL1Shift = arenaL2Bits

	// arenaBits is the total bits in a combined arena map index.
	// This is split between the index into the L1 arena map and
	// the L2 arena map.
	arenaBits = arenaL1Bits + arenaL2Bits

	// arenaBaseOffset is the pointer value that corresponds to
	// index 0 in the heap arena map.
	//
	// On amd64, the address space is 48 bits, sign extended to 64
	// bits. This offset lets us handle "negative" addresses (or
	// high addresses if viewed as unsigned).
	//
	// On other platforms, the user address space is contiguous
	// and starts at 0, so no offset is necessary.
	arenaBaseOffset uintptr = sys.GoarchAmd64 * (1 << 47)

	// Max number of threads to run garbage collection.
	// 2, 3, and 4 are all plausible maximums depending
	// on the hardware details of the machine. The garbage
	// collector scales well to 32 cpus.
	_MaxGcproc = 32

	// minLegalPointer is the smallest possible legal pointer.
	// This is the smallest possible architectural page size,
	// since we assume that the first page is never mapped.
	//
	// This should agree with minZeroPage in the compiler.
	minLegalPointer uintptr = 4096
)

// physPageSize is the size in bytes of the OS's physical pages.
// Mapping and unmapping operations must be done at multiples of
// physPageSize.
//
// This must be set by the OS init code (typically in osinit) before
// mallocinit.
// 操作系统的物理页大小，以字节为单位
var physPageSize uintptr

// physHugePageSize is the size in bytes of the OS's default physical huge
// page size whose allocation is opaque to the application. It is assumed
// and verified to be a power of two.
//
// If set, this must be set by the OS init code (typically in osinit) before
// mallocinit. However, setting it at all is optional, and leaving the default
// value is always safe (though potentially less efficient).
//
// Since physHugePageSize is always assumed to be a power of two,
// physHugePageShift is defined as physHugePageSize == 1 << physHugePageShift.
// The purpose of physHugePageShift is to avoid doing divisions in
// performance critical functions.
var (
	physHugePageSize  uintptr
	physHugePageShift uint
)

// OS memory management abstraction layer
//
// Regions of the address space managed by the runtime may be in one of four
// states at any given time:
// 1) None - Unreserved and unmapped, the default state of any region.
// 2) Reserved - Owned by the runtime, but accessing it would cause a fault.
//               Does not count against the process' memory footprint.
// 3) Prepared - Reserved, intended not to be backed by physical memory (though
//               an OS may implement this lazily). Can transition efficiently to
//               Ready. Accessing memory in such a region is undefined (may
//               fault, may give back unexpected zeroes, etc.).
// 4) Ready - may be accessed safely.
//
// This set of states is more than is strictly necessary to support all the
// currently supported platforms. One could get by with just None, Reserved, and
// Ready. However, the Prepared state gives us flexibility for performance
// purposes. For example, on POSIX-y operating systems, Reserved is usually a
// private anonymous mmap'd region with PROT_NONE set, and to transition
// to Ready would require setting PROT_READ|PROT_WRITE. However the
// underspecification of Prepared lets us use just MADV_FREE to transition from
// Ready to Prepared. Thus with the Prepared state we can set the permission
// bits just once early on, we can efficiently tell the OS that it's free to
// take pages away from us when we don't strictly need them.
//
// For each OS there is a common set of helpers defined that transition
// memory regions between these states. The helpers are as follows:
//
// sysAlloc transitions an OS-chosen region of memory from None to Ready.
// More specifically, it obtains a large chunk of zeroed memory from the
// operating system, typically on the order of a hundred kilobytes
// or a megabyte. This memory is always immediately available for use.
//
// sysFree transitions a memory region from any state to None. Therefore, it
// returns memory unconditionally. It is used if an out-of-memory error has been
// detected midway through an allocation or to carve out an aligned section of
// the address space. It is okay if sysFree is a no-op only if sysReserve always
// returns a memory region aligned to the heap allocator's alignment
// restrictions.
//
// sysReserve transitions a memory region from None to Reserved. It reserves
// address space in such a way that it would cause a fatal fault upon access
// (either via permissions or not committing the memory). Such a reservation is
// thus never backed by physical memory.
// If the pointer passed to it is non-nil, the caller wants the
// reservation there, but sysReserve can still choose another
// location if that one is unavailable.
// NOTE: sysReserve returns OS-aligned memory, but the heap allocator
// may use larger alignment, so the caller must be careful to realign the
// memory obtained by sysReserve.
//
// sysMap transitions a memory region from Reserved to Prepared. It ensures the
// memory region can be efficiently transitioned to Ready.
//
// sysUsed transitions a memory region from Prepared to Ready. It notifies the
// operating system that the memory region is needed and ensures that the region
// may be safely accessed. This is typically a no-op on systems that don't have
// an explicit commit step and hard over-commit limits, but is critical on
// Windows, for example.
//
// sysUnused transitions a memory region from Ready to Prepared. It notifies the
// operating system that the physical pages backing this memory region are no
// longer needed and can be reused for other purposes. The contents of a
// sysUnused memory region are considered forfeit and the region must not be
// accessed again until sysUsed is called.
//
// sysFault transitions a memory region from Ready or Prepared to Reserved. It
// marks a region such that it will always fault if accessed. Used only for
// debugging the runtime.

// 注释摘至 https://github.com/cch123/golang-notes/blob/master/memory.md
// sysAlloc 从操作系统获取一大块已清零的内存，一般是 100 KB 或 1MB
// NOTE: sysAlloc 返回 OS 对齐的内存，但是对于堆分配器来说可能需要以更大的单位进行对齐。
// 因此 caller 需要小心地将 sysAlloc 获取到的内存重新进行对齐。
//
// sysUnused 通知操作系统内存区域的内容已经没用了，可以移作它用。
// sysUsed 通知操作系统内存区域的内容又需要用了。
//
// sysFree 无条件返回内存；只有当分配内存途中发生了 out-of-memory 错误
// 时才会使用。如果 sysFree 本身啥也没干成(no-op)也是 ok 的。
//
// sysReserve 会在不分配内存的情况下，保留一段地址空间。
// 如果传给它的指针是非 nil，意思是 caller 想保留这段地址，
// 但这种情况下，如果该段地址不可用时，sysReserve 依然可以选择另外的地址。
// 在一些操作系统的某些 case 下，sysReserve 化合简单地检查这段地址空间是否可用
// 同时并不会真地保留它。sysReserve 返回非空指针时，如果地址空间保留成功了
// 会将 *reserved 设置为 true，只是检查而未保留的话会设置为 false。
// NOTE: sysReserve 返回 系统对齐的内存，没有按堆分配器的更大对齐单位进行对齐，
// 所以 caller 需要将通过 sysAlloc 获取到的内存进行重对齐。
//
// sysMap 将之前保留的地址空间映射好以进行使用。
// 如果地址空间确实被保留的话，reserved 参数会为 true。只 check 的话是 false。
//
// sysFault 当一块内存已经被 sysAlloc 时，标记其为 fault。只在 debug runtime 的时候用用。

func mallocinit() {
	if class_to_size[_TinySizeClass] != _TinySize {
		throw("bad TinySizeClass")
	}

	testdefersizes()

	// heapArenaBitmapBytes只有1位为1 其余bit位为0，我本地测试的heapArenaBitmapBytes为2097152=2M
	if heapArenaBitmapBytes&(heapArenaBitmapBytes-1) != 0 {
		// heapBits expects modular arithmetic on bitmap
		// addresses to work.
		throw("heapArenaBitmapBytes not a power of 2")
	}

	// Copy class sizes out for statistics table.
	for i := range class_to_size {
		memstats.by_size[i].size = uint32(class_to_size[i])
	}

	// Check physPageSize.
	if physPageSize == 0 {
		// The OS init code failed to fetch the physical page size.
		throw("failed to get system page size")
	}
	if physPageSize < minPhysPageSize {
		print("system page size (", physPageSize, ") is smaller than minimum page size (", minPhysPageSize, ")\n")
		throw("bad system page size")
	}
	if physPageSize&(physPageSize-1) != 0 {
		print("system page size (", physPageSize, ") must be a power of 2\n")
		throw("bad system page size")
	}
	if physHugePageSize&(physHugePageSize-1) != 0 {
		print("system huge page size (", physHugePageSize, ") must be a power of 2\n")
		throw("bad system huge page size")
	}
	if physHugePageSize != 0 {
		// Since physHugePageSize is a power of 2, it suffices to increase
		// physHugePageShift until 1<<physHugePageShift == physHugePageSize.
		for 1<<physHugePageShift != physHugePageSize {
			physHugePageShift++
		}
	}

	// Initialize the heap.
	// 初始化堆
	mheap_.init()
	// 获取当前g, 此时是g0，获取的也是m0， m0的mcache是在初始化的时候分配
	_g_ := getg()

	// 初始化g0.m0.mcache
	_g_.m.mcache = allocmcache()

	// Create initial arena growth hints.
	// 如果系统指针是8字节，64位系统，创建初始的 arena 增长 hint
	if sys.PtrSize == 8 {
		// On a 64-bit machine, we pick the following hints
		// because:
		//
		// 1. Starting from the middle of the address space
		// makes it easier to grow out a contiguous range
		// without running in to some other mapping.
		//
		// 2. This makes Go heap addresses more easily
		// recognizable when debugging.
		//
		// 3. Stack scanning in gccgo is still conservative,
		// so it's important that addresses be distinguishable
		// from other data.
		//
		// Starting at 0x00c0 means that the valid memory addresses
		// will begin 0x00c0, 0x00c1, ...
		// In little-endian, that's c0 00, c1 00, ... None of those are valid
		// UTF-8 sequences, and they are otherwise as far away from
		// ff (likely a common byte) as possible. If that fails, we try other 0xXXc0
		// addresses. An earlier attempt to use 0x11f8 caused out of memory errors
		// on OS X during thread allocations.  0x00c0 causes conflicts with
		// AddressSanitizer which reserves all memory up to 0x0100.
		// These choices reduce the odds of a conservative garbage collector
		// not collecting memory because some non-pointer block of memory
		// had a bit pattern that matched a memory address.
		//
		// However, on arm64, we ignore all this advice above and slam the
		// allocation at 0x40 << 32 because when using 4k pages with 3-level
		// translation buffers, the user address space is limited to 39 bits
		// On darwin/arm64, the address space is even smaller.
		// On AIX, mmaps starts at 0x0A00000000000000 for 64-bit.
		// processes.
		// 当前版本的AMD64架构就规定了只用48位地址；一个表示虚拟内存地址的64位指针只有低48位有效并带符号扩展到64位——换句话说，其高16位必须是全1或全0，而且必须与低48位的最高位（第47位）一致，否则通过该地址访问内存会产生#GP异常（general-protection exception）
		// 因此此处的循环从0X7f开始，这样高16位为0， 低48位的高位也为0，真的处处都是学问呀
		// 将00C000000000 ~ 7FC000000000作为保留地址挂到mheap_.arenaHints上
		for i := 0x7f; i >= 0; i-- {
			var p uintptr
			switch {
			case GOARCH == "arm64" && GOOS == "darwin":
				p = uintptr(i)<<40 | uintptrMask&(0x0013<<28)
			case GOARCH == "arm64":
				p = uintptr(i)<<40 | uintptrMask&(0x0040<<32)
			case GOOS == "aix":
				if i == 0 {
					// We don't use addresses directly after 0x0A00000000000000
					// to avoid collisions with others mmaps done by non-go programs.
					continue
				}
				p = uintptr(i)<<40 | uintptrMask&(0xa0<<52)
			case raceenabled:
				// The TSAN runtime requires the heap
				// to be in the range [0x00c000000000,
				// 0x00e000000000).
				p = uintptr(i)<<32 | uintptrMask&(0x00c0<<32)
				if p >= uintptrMask&0x00e000000000 {
					continue
				}
			default:
				p = uintptr(i)<<40 | uintptrMask&(0x00c0<<32)
			}
			hint := (*arenaHint)(mheap_.arenaHintAlloc.alloc())
			hint.addr = p
			hint.next, mheap_.arenaHints = mheap_.arenaHints, hint
		}
	} else {
		// On a 32-bit machine, we're much more concerned
		// about keeping the usable heap contiguous.
		// Hence:
		//
		// 1. We reserve space for all heapArenas up front so
		// they don't get interleaved with the heap. They're
		// ~258MB, so this isn't too bad. (We could reserve a
		// smaller amount of space up front if this is a
		// problem.)
		//
		// 2. We hint the heap to start right above the end of
		// the binary so we have the best chance of keeping it
		// contiguous.
		//
		// 3. We try to stake out a reasonably large initial
		// heap reservation.

		const arenaMetaSize = (1 << arenaBits) * unsafe.Sizeof(heapArena{})
		meta := uintptr(sysReserve(nil, arenaMetaSize))
		if meta != 0 {
			mheap_.heapArenaAlloc.init(meta, arenaMetaSize)
		}

		// We want to start the arena low, but if we're linked
		// against C code, it's possible global constructors
		// have called malloc and adjusted the process' brk.
		// Query the brk so we can avoid trying to map the
		// region over it (which will cause the kernel to put
		// the region somewhere else, likely at a high
		// address).
		procBrk := sbrk0()

		// If we ask for the end of the data segment but the
		// operating system requires a little more space
		// before we can start allocating, it will give out a
		// slightly higher pointer. Except QEMU, which is
		// buggy, as usual: it won't adjust the pointer
		// upward. So adjust it upward a little bit ourselves:
		// 1/4 MB to get away from the running binary image.
		p := firstmoduledata.end
		if p < procBrk {
			p = procBrk
		}
		if mheap_.heapArenaAlloc.next <= p && p < mheap_.heapArenaAlloc.end {
			p = mheap_.heapArenaAlloc.end
		}
		p = round(p+(256<<10), heapArenaBytes)
		// Because we're worried about fragmentation on
		// 32-bit, we try to make a large initial reservation.
		arenaSizes := []uintptr{
			512 << 20,
			256 << 20,
			128 << 20,
		}
		for _, arenaSize := range arenaSizes {
			a, size := sysReserveAligned(unsafe.Pointer(p), arenaSize, heapArenaBytes)
			if a != nil {
				mheap_.arena.init(uintptr(a), size)
				p = uintptr(a) + size // For hint below
				break
			}
		}
		hint := (*arenaHint)(mheap_.arenaHintAlloc.alloc())
		hint.addr = p
		hint.next, mheap_.arenaHints = mheap_.arenaHints, hint
	}
}

// sysAlloc allocates heap arena space for at least n bytes. The
// returned pointer is always heapArenaBytes-aligned and backed by
// h.arenas metadata. The returned size is always a multiple of
// heapArenaBytes. sysAlloc returns nil on failure.
// There is no corresponding free function.
//
// sysAlloc returns a memory region in the Prepared state. This region must
// be transitioned to Ready before use.
//
// h must be locked.
//
// 申请至少n byte的内存， 通常是64M的整数倍
func (h *mheap) sysAlloc(n uintptr) (v unsafe.Pointer, size uintptr) {
	// n向64M取整
	n = round(n, heapArenaBytes)

	// First, try the arena pre-reservation.
	// 从 arena 中 获取对应大小的内存， 获取不到返回nil， 在64位系统上一直返回nil
	v = h.arena.alloc(n, heapArenaBytes, &memstats.heap_sys)
	if v != nil {
		size = n
		goto mapped
	}

	// Try to grow the heap at a hint address.
	// 从arena获取到需要的内存，跳转到 mapped操作
	for h.arenaHints != nil {
		hint := h.arenaHints
		// 当前arena的虚拟地址的起始地址
		p := hint.addr
		// 如果是最后一个
		if hint.down {
			p -= n
		}

		if p+n < p {
			// 溢出了 或者hint.down=true，不能向下扩展，因此不能使用
			// We can't use this, so don't ask.
			v = nil
		} else if arenaIndex(p+n-1) >= 1<<arenaBits { // 溢出区域到高位了， 也不能分配了  linux中arenaBits为22
			// 22bit是4M 4M*heapArenaBytes（64M） = 256TB 就是用了256T的空间
			// 已经超出了head的寻址空间了
			// Outside addressable heap. Can't use.
			v = nil
		} else {
			// 从当前位置p，保留一段长度为n的内存，如果申请成功返回了指针 v不一定等于p
			v = sysReserve(unsafe.Pointer(p), n)
		}
		// 如果申请的位置跟p是一个位置，申请成功，修改当前hint的起始地址
		if p == uintptr(v) {
			// Success. Update the hint.
			if !hint.down {
				p += n
			}
			hint.addr = p
			size = n
			break
		}
		// Failed. Discard this hint and try the next.
		//
		// TODO: This would be cleaner if sysReserve could be
		// told to only return the requested address. In
		// particular, this is already how Windows behaves, so
		// it would simplify things there.
		if v != nil {
			// 由于申请v和我要的p位置不相同，还回去
			sysFree(v, n, nil)
		}
		// 换到下一个增长空间
		h.arenaHints = hint.next
		// 上一个hint已经分配完了，释放掉
		h.arenaHintAlloc.free(unsafe.Pointer(hint))
	}

	// 如果遍历了所有的arenaHints，还没有找到
	if size == 0 {
		if raceenabled {
			// The race detector assumes the heap lives in
			// [0x00c000000000, 0x00e000000000), but we
			// just ran out of hints in this region. Give
			// a nice failure.
			throw("too many address space collisions for -race mode")
		}

		// All of the hints failed, so we'll take any
		// (sufficiently aligned) address the kernel will give
		// us.
		// 所有的hints都没有了，直接向内核要吧，申请一个64MB的空间
		v, size = sysReserveAligned(nil, n, heapArenaBytes)
		// 如果还申请不到，申请失败
		if v == nil {
			return nil, 0
		}

		// Create new hints for extending this region.
		// 创建一个新的对象，挂在arenaHints上，并且标识其为最后一个
		hint := (*arenaHint)(h.arenaHintAlloc.alloc())
		hint.addr, hint.down = uintptr(v), true
		hint.next, mheap_.arenaHints = mheap_.arenaHints, hint
		hint = (*arenaHint)(h.arenaHintAlloc.alloc())
		hint.addr = uintptr(v) + size
		hint.next, mheap_.arenaHints = mheap_.arenaHints, hint
	}

	// Check for bad pointers or pointers we can't use.
	// 检查返回的指针是不是有问题
	{
		var bad string
		p := uintptr(v)
		if p+size < p {
			bad = "region exceeds uintptr range"
		} else if arenaIndex(p) >= 1<<arenaBits {
			bad = "base outside usable address space"
		} else if arenaIndex(p+size-1) >= 1<<arenaBits {
			bad = "end outside usable address space"
		}
		if bad != "" {
			// This should be impossible on most architectures,
			// but it would be really confusing to debug.
			print("runtime: memory allocated by OS [", hex(p), ", ", hex(p+size), ") not in usable address space: ", bad, "\n")
			throw("memory reservation exceeds address space limit")
		}
	}

	if uintptr(v)&(heapArenaBytes-1) != 0 {
		throw("misrounded allocation in sysAlloc")
	}

	// Transition from Reserved to Prepared.
	// 将内存标记为可以使用了
	sysMap(v, size, &memstats.heap_sys)

mapped:
	// Create arena metadata.
	// 创建一个arena
	// 根据 v 的address，计算出 arenas 的L1 L2， 申请了多少个arean就添加几个，通常为1个
	for ri := arenaIndex(uintptr(v)); ri <= arenaIndex(uintptr(v)+size-1); ri++ { // amd64下l1=0
		l2 := h.arenas[ri.l1()]
		if l2 == nil {
			// 初始化l2的arena map 等价于golang中的h.arenas[ri.l1()] = [1 << arenaL2Bits]*heapArena
			// 为什么没有这么做呢？
			// Allocate an L2 arena map.
			l2 = (*[1 << arenaL2Bits]*heapArena)(persistentalloc(unsafe.Sizeof(*l2), sys.PtrSize, nil))
			if l2 == nil {
				throw("out of memory allocating heap arena map")
			}
			atomic.StorepNoWB(unsafe.Pointer(&h.arenas[ri.l1()]), unsafe.Pointer(l2))
		}

		if l2[ri.l2()] != nil {
			throw("arena already initialized")
		}
		var r *heapArena
		r = (*heapArena)(h.heapArenaAlloc.alloc(unsafe.Sizeof(*r), sys.PtrSize, &memstats.gc_sys))
		// 在64位系统上，没有heapArenaAlloc的分配器
		if r == nil {
			r = (*heapArena)(persistentalloc(unsafe.Sizeof(*r), sys.PtrSize, &memstats.gc_sys))
			if r == nil {
				throw("out of memory allocating heap arena metadata")
			}
		}

		// Add the arena to the arenas list.
		// 将创建的arena添加到arenas list中
		// 如果当前allArenas满了，双倍扩容
		if len(h.allArenas) == cap(h.allArenas) {
			// 翻倍
			size := 2 * uintptr(cap(h.allArenas)) * sys.PtrSize
			if size == 0 {
				// 如果是第一次 创建个物理页大小
				size = physPageSize
			}
			newArray := (*notInHeap)(persistentalloc(size, sys.PtrSize, &memstats.gc_sys))
			if newArray == nil {
				throw("out of memory allocating allArenas")
			}
			oldSlice := h.allArenas
			*(*notInHeapSlice)(unsafe.Pointer(&h.allArenas)) = notInHeapSlice{newArray, len(h.allArenas), int(size / sys.PtrSize)}
			copy(h.allArenas, oldSlice)
			// Do not free the old backing array because
			// there may be concurrent readers. Since we
			// double the array each time, this can lead
			// to at most 2x waste.
		}
		// 将ri添加到allArenas的末尾
		h.allArenas = h.allArenas[:len(h.allArenas)+1]
		h.allArenas[len(h.allArenas)-1] = ri

		// Store atomically just in case an object from the
		// new heap arena becomes visible before the heap lock
		// is released (which shouldn't happen, but there's
		// little downside to this).
		atomic.StorepNoWB(unsafe.Pointer(&l2[ri.l2()]), unsafe.Pointer(r))
	}

	// Tell the race detector about the new heap memory.
	if raceenabled {
		racemapshadow(v, size)
	}

	return
}

// sysReserveAligned is like sysReserve, but the returned pointer is
// aligned to align bytes. It may reserve either n or n+align bytes,
// so it returns the size that was reserved.
// 保留一段对齐后的地址，返回地址和内存的大小
func sysReserveAligned(v unsafe.Pointer, size, align uintptr) (unsafe.Pointer, uintptr) {
	// Since the alignment is rather large in uses of this
	// function, we're not likely to get it by chance, so we ask
	// for a larger region and remove the parts we don't need.
	retries := 0
retry:
	p := uintptr(sysReserve(v, size+align))
	switch {
	case p == 0:
		return nil, 0
	case p&(align-1) == 0:
		// We got lucky and got an aligned region, so we can
		// use the whole thing.
		return unsafe.Pointer(p), size + align
	case GOOS == "windows":
		// On Windows we can't release pieces of a
		// reservation, so we release the whole thing and
		// re-reserve the aligned sub-region. This may race,
		// so we may have to try again.
		sysFree(unsafe.Pointer(p), size+align, nil)
		p = round(p, align)
		p2 := sysReserve(unsafe.Pointer(p), size)
		if p != uintptr(p2) {
			// Must have raced. Try again.
			sysFree(p2, size, nil)
			if retries++; retries == 100 {
				throw("failed to allocate aligned heap memory; too many retries")
			}
			goto retry
		}
		// Success.
		return p2, size
	default:
		// Trim off the unaligned parts.
		pAligned := round(p, align)
		sysFree(unsafe.Pointer(p), pAligned-p, nil)
		end := pAligned + size
		endLen := (p + size + align) - end
		if endLen > 0 {
			sysFree(unsafe.Pointer(end), endLen, nil)
		}
		return unsafe.Pointer(pAligned), size
	}
}

// base address for all 0-byte allocations
// 当申请的一个0byte的时候，直接返回zerobase
var zerobase uintptr

// nextFreeFast returns the next free object if one is quickly available.
// Otherwise it returns 0.
// 返回下一个可用的空地址空间，如果没找到返回0
func nextFreeFast(s *mspan) gclinkptr {
	// 第几位开始不是0,最开始macahe中的span都为emptyspan，初始化时allocCache为0，所以返回的是64个
	// 调用nextFree后会或者一个新的span，初始化时allocache= ^uint64(0)，此时theBit从0开始
	theBit := sys.Ctz64(s.allocCache) // Is there a free object in the allocCache?
	if theBit < 64 {
		result := s.freeindex + uintptr(theBit)
		if result < s.nelems {
			freeidx := result + 1
			if freeidx%64 == 0 && freeidx != s.nelems {
				return 0
			}
			s.allocCache >>= uint(theBit + 1)
			s.freeindex = freeidx
			s.allocCount++
			return gclinkptr(result*s.elemsize + s.base())
		}
	}
	return 0
}

// nextFree returns the next free object from the cached span if one is available.
// Otherwise it refills the cache with a span with an available object and
// returns that object along with a flag indicating that this was a heavy
// weight allocation. If it is a heavy weight allocation the caller must
// determine whether a new GC cycle needs to be started or if the GC is active
// whether this goroutine needs to assist the GC.
//
// Must run in a non-preemptible context since otherwise the owner of
// c could change.
//
func (c *mcache) nextFree(spc spanClass) (v gclinkptr, s *mspan, shouldhelpgc bool) {
	// 选择span
	s = c.alloc[spc]
	shouldhelpgc = false
	freeIndex := s.nextFreeIndex()
	// span 满了 或者span是个emptySpan
	if freeIndex == s.nelems {
		// The span is full.
		if uintptr(s.allocCount) != s.nelems {
			println("runtime: s.allocCount=", s.allocCount, "s.nelems=", s.nelems)
			throw("s.allocCount != s.nelems && freeIndex == s.nelems")
		}
		// 重新获取一个spc类型的span
		c.refill(spc)
		shouldhelpgc = true
		s = c.alloc[spc]

		// 再找一次下一个元素
		freeIndex = s.nextFreeIndex()
	}

	if freeIndex >= s.nelems {
		throw("freeIndex is not valid")
	}

	v = gclinkptr(freeIndex*s.elemsize + s.base())
	// 分配次数+1
	s.allocCount++
	if uintptr(s.allocCount) > s.nelems {
		println("s.allocCount=", s.allocCount, "s.nelems=", s.nelems)
		throw("s.allocCount > s.nelems")
	}
	return
}

// Allocate an object of size bytes.
// Small objects are allocated from the per-P cache's free lists.
// Large objects (> 32 kB) are allocated straight from the heap.
// 分配一个指定大小的对象
func mallocgc(size uintptr, typ *_type, needzero bool) unsafe.Pointer {
	if gcphase == _GCmarktermination {
		throw("mallocgc called with gcphase == _GCmarktermination")
	}

	if size == 0 {
		return unsafe.Pointer(&zerobase)
	}

	if debug.sbrk != 0 {
		align := uintptr(16)
		if typ != nil {
			// TODO(austin): This should be just
			//   align = uintptr(typ.align)
			// but that's only 4 on 32-bit platforms,
			// even if there's a uint64 field in typ (see #599).
			// This causes 64-bit atomic accesses to panic.
			// Hence, we use stricter alignment that matches
			// the normal allocator better.
			if size&7 == 0 {
				align = 8
			} else if size&3 == 0 {
				align = 4
			} else if size&1 == 0 {
				align = 2
			} else {
				align = 1
			}
		}
		return persistentalloc(size, align, &memstats.other_sys)
	}

	// assistG is the G to charge for this allocation, or nil if
	// GC is not currently active.
	// 当前的g，如果gc当前不是活跃状态，则g==nil
	var assistG *g
	// 如果没开启后台标记
	if gcBlackenEnabled != 0 {
		// Charge the current user G for this allocation.
		// 让出资源
		assistG = getg()
		if assistG.m.curg != nil {
			assistG = assistG.m.curg
		}
		// Charge the allocation against the G. We'll account
		// for internal fragmentation at the end of mallocgc.
		assistG.gcAssistBytes -= int64(size)

		if assistG.gcAssistBytes < 0 {
			// This G is in debt. Assist the GC to correct
			// this before allocating. This must happen
			// before disabling preemption.
			// 参与回收任务
			gcAssistAlloc(assistG)
		}
	}

	// Set mp.mallocing to keep from being preempted by GC.
	// 当前m禁止抢占
	mp := acquirem()
	if mp.mallocing != 0 {
		throw("malloc deadlock")
	}
	if mp.gsignal == getg() {
		throw("malloc during signal")
	}
	mp.mallocing = 1

	shouldhelpgc := false
	dataSize := size
	c := gomcache()
	var x unsafe.Pointer
	// typ==nil || 没有指针 则对用不用扫描
	noscan := typ == nil || typ.ptrdata == 0

	// 如果不是大的对象  size<32K
	if size <= maxSmallSize {
		// 如果是一个微小对象 size<16 并且不是指针
		// string.go@rawbyteslice就有这种微小对象
		if noscan && size < maxTinySize {
			// Tiny allocator.
			//
			// Tiny allocator combines several tiny allocation requests
			// into a single memory block. The resulting memory block
			// is freed when all subobjects are unreachable. The subobjects
			// must be noscan (don't have pointers), this ensures that
			// the amount of potentially wasted memory is bounded.
			//
			// Size of the memory block used for combining (maxTinySize) is tunable.
			// Current setting is 16 bytes, which relates to 2x worst case memory
			// wastage (when all but one subobjects are unreachable).
			// 8 bytes would result in no wastage at all, but provides less
			// opportunities for combining.
			// 32 bytes provides more opportunities for combining,
			// but can lead to 4x worst case wastage.
			// The best case winning is 8x regardless of block size.
			//
			// Objects obtained from tiny allocator must not be freed explicitly.
			// So when an object will be freed explicitly, we ensure that
			// its size >= maxTinySize.
			//
			// SetFinalizer has a special case for objects potentially coming
			// from tiny allocator, it such case it allows to set finalizers
			// for an inner byte of a memory block.
			//
			// The main targets of tiny allocator are small strings and
			// standalone escaping variables. On a json benchmark
			// the allocator reduces number of allocations by ~12% and
			// reduces heap size by ~20%.
			// 如果mcache是第一次使用，tinyoffset为0
			off := c.tinyoffset
			// 进行对齐
			// Align tiny pointer for required (conservative) alignment.
			// 判断对齐位数，是8位还是4位还是2位
			if size&7 == 0 {
				off = round(off, 8)
			} else if size&3 == 0 {
				off = round(off, 4)
			} else if size&1 == 0 {
				off = round(off, 2)
			}

			// 如果偏移地址和申请的size  < 16 并且 c.tiny已经分配过了
			if off+size <= maxTinySize && c.tiny != 0 {
				// The object fits into existing tiny block.
				// 那就是说现在申请的这个对象适合这个微小对象，那么当前申请的位置就行c.tiny+off
				x = unsafe.Pointer(c.tiny + off)
				// tinyoffset添加一个地址
				c.tinyoffset = off + size
				// 本地的微小对象分配次数加1
				c.local_tinyallocs++
				mp.mallocing = 0
				releasem(mp)
				return x
			}
			// Allocate a new maxTinySize block.
			// 找到maxTinySize对应的块， tinySpanClass=5
			// class_to_size中对应16字节的index是3，由于微小对象肯定没有指针，而没有指针的结构按1、3、5序列排列，因此16字节对应index=5
			span := c.alloc[tinySpanClass]

			// 从当前的span获取
			v := nextFreeFast(span)
			// 当前span没有，如果快速获取失败，则通过普通获取
			if v == 0 {
				v, _, shouldhelpgc = c.nextFree(tinySpanClass)
			}
			x = unsafe.Pointer(v)
			(*[2]uint64)(x)[0] = 0
			(*[2]uint64)(x)[1] = 0
			// See if we need to replace the existing tiny block with the new one
			// based on amount of remaining free space.
			if size < c.tinyoffset || c.tiny == 0 {
				c.tiny = uintptr(x)
				c.tinyoffset = size
			}
			size = maxTinySize
		} else { // 小对象
			var sizeclass uint8
			if size <= smallSizeMax-8 {
				sizeclass = size_to_class8[(size+smallSizeDiv-1)/smallSizeDiv]
			} else {
				sizeclass = size_to_class128[(size-smallSizeMax+largeSizeDiv-1)/largeSizeDiv]
			}
			size = uintptr(class_to_size[sizeclass])
			spc := makeSpanClass(sizeclass, noscan)
			span := c.alloc[spc]
			v := nextFreeFast(span)
			if v == 0 {
				v, span, shouldhelpgc = c.nextFree(spc)
			}
			x = unsafe.Pointer(v)
			if needzero && span.needzero != 0 {
				memclrNoHeapPointers(unsafe.Pointer(v), size)
			}
		}
	} else { //大对象
		var s *mspan
		shouldhelpgc = true
		systemstack(func() {
			// 分配一个大对象
			s = largeAlloc(size, needzero, noscan)
		})
		s.freeindex = 1
		s.allocCount = 1
		x = unsafe.Pointer(s.base())
		size = s.elemsize
	}

	var scanSize uintptr
	if !noscan {
		// If allocating a defer+arg block, now that we've picked a malloc size
		// large enough to hold everything, cut the "asked for" size down to
		// just the defer header, so that the GC bitmap will record the arg block
		// as containing nothing at all (as if it were unused space at the end of
		// a malloc block caused by size rounding).
		// The defer arg areas are scanned as part of scanstack.
		if typ == deferType {
			dataSize = unsafe.Sizeof(_defer{})
		}
		heapBitsSetType(uintptr(x), size, dataSize, typ)
		if dataSize > typ.size {
			// Array allocation. If there are any
			// pointers, GC has to scan to the last
			// element.
			if typ.ptrdata != 0 {
				scanSize = dataSize - typ.size + typ.ptrdata
			}
		} else {
			scanSize = typ.ptrdata
		}
		c.local_scan += scanSize
	}

	// Ensure that the stores above that initialize x to
	// type-safe memory and set the heap bits occur before
	// the caller can make x observable to the garbage
	// collector. Otherwise, on weakly ordered machines,
	// the garbage collector could follow a pointer to x,
	// but see uninitialized memory or stale heap bits.
	publicationBarrier()

	// Allocate black during GC.
	// All slots hold nil so no scanning is needed.
	// This may be racing with GC so do it atomically if there can be
	// a race marking the bit.
	// 如果当前gc的状态不是gcoff，则新增的变量标记为黑色
	if gcphase != _GCoff {
		gcmarknewobject(uintptr(x), size, scanSize)
	}

	if raceenabled {
		racemalloc(x, size)
	}

	if msanenabled {
		msanmalloc(x, size)
	}

	mp.mallocing = 0
	releasem(mp)

	if debug.allocfreetrace != 0 {
		tracealloc(x, size, typ)
	}

	if rate := MemProfileRate; rate > 0 {
		if rate != 1 && size < c.next_sample {
			c.next_sample -= size
		} else {
			mp := acquirem()
			profilealloc(mp, x, size)
			releasem(mp)
		}
	}

	if assistG != nil {
		// Account for internal fragmentation in the assist
		// debt now that we know it.
		assistG.gcAssistBytes -= int64(size - dataSize)
	}

	// 检查是否需要gc
	if shouldhelpgc {
		if t := (gcTrigger{kind: gcTriggerHeap}); t.test() {
			gcStart(t)
		}
	}

	return x
}

// 大对象的分配，size是大小，needzero是否清零  noscan是gc相关的，暂时不知道
func largeAlloc(size uintptr, needzero bool, noscan bool) *mspan {
	// print("largeAlloc size=", size, "\n")

	// 溢出了
	if size+_PageSize < size {
		throw("out of memory")
	}

	// 根据size计算需要分配的页数
	npages := size >> _PageShift
	if size&_PageMask != 0 {
		// size不能整除pageshift，所以移位操作会丢弃一部分，此时需要+1
		npages++
	}

	// Deduct credit for this span allocation and sweep if
	// necessary. mHeap_Alloc will also sweep npages, so this only
	// pays the debt down to npage pages.
	deductSweepCredit(npages*_PageSize, npages)

	// 从堆上分配
	s := mheap_.alloc(npages, makeSpanClass(0, noscan), true, needzero)
	if s == nil {
		throw("out of memory")
	}
	s.limit = s.base() + size
	heapBitsForAddr(s.base()).initSpan(s)
	return s
}

// implementation of new builtin
// compiler (both frontend and SSA backend) knows the signature
// of this function
// 所有堆的内存分配都通过此函数
// 由此函数分配的对象，我们称之为逃逸
func newobject(typ *_type) unsafe.Pointer {
	return mallocgc(typ.size, typ, true)
}

//go:linkname reflect_unsafe_New reflect.unsafe_New
func reflect_unsafe_New(typ *_type) unsafe.Pointer {
	return mallocgc(typ.size, typ, true)
}

//go:linkname reflectlite_unsafe_New internal/reflectlite.unsafe_New
func reflectlite_unsafe_New(typ *_type) unsafe.Pointer {
	return mallocgc(typ.size, typ, true)
}

// newarray allocates an array of n elements of type typ.
func newarray(typ *_type, n int) unsafe.Pointer {
	if n == 1 {
		return mallocgc(typ.size, typ, true)
	}
	mem, overflow := math.MulUintptr(typ.size, uintptr(n))
	if overflow || mem > maxAlloc || n < 0 {
		panic(plainError("runtime: allocation size out of range"))
	}
	return mallocgc(mem, typ, true)
}

//go:linkname reflect_unsafe_NewArray reflect.unsafe_NewArray
func reflect_unsafe_NewArray(typ *_type, n int) unsafe.Pointer {
	return newarray(typ, n)
}

func profilealloc(mp *m, x unsafe.Pointer, size uintptr) {
	mp.mcache.next_sample = nextSample()
	mProf_Malloc(x, size)
}

// nextSample returns the next sampling point for heap profiling. The goal is
// to sample allocations on average every MemProfileRate bytes, but with a
// completely random distribution over the allocation timeline; this
// corresponds to a Poisson process with parameter MemProfileRate. In Poisson
// processes, the distance between two samples follows the exponential
// distribution (exp(MemProfileRate)), so the best return value is a random
// number taken from an exponential distribution whose mean is MemProfileRate.
// 返回下一个堆分析的采样点，是服从泊松过程的随机数
func nextSample() uintptr {
	if GOOS == "plan9" {
		// Plan 9 doesn't support floating point in note handler.
		if g := getg(); g == g.m.gsignal {
			return nextSampleNoFP()
		}
	}

	return uintptr(fastexprand(MemProfileRate))
}

// fastexprand returns a random number from an exponential distribution with
// the specified mean.
func fastexprand(mean int) int32 {
	// Avoid overflow. Maximum possible step is
	// -ln(1/(1<<randomBitCount)) * mean, approximately 20 * mean.
	switch {
	case mean > 0x7000000:
		mean = 0x7000000
	case mean == 0:
		return 0
	}

	// Take a random sample of the exponential distribution exp(-mean*x).
	// The probability distribution function is mean*exp(-mean*x), so the CDF is
	// p = 1 - exp(-mean*x), so
	// q = 1 - p == exp(-mean*x)
	// log_e(q) = -mean*x
	// -log_e(q)/mean = x
	// x = -log_e(q) * mean
	// x = log_2(q) * (-log_e(2)) * mean    ; Using log_2 for efficiency
	const randomBitCount = 26
	q := fastrand()%(1<<randomBitCount) + 1
	qlog := fastlog2(float64(q)) - randomBitCount
	if qlog > 0 {
		qlog = 0
	}
	const minusLog2 = -0.6931471805599453 // -ln(2)
	return int32(qlog*(minusLog2*float64(mean))) + 1
}

// nextSampleNoFP is similar to nextSample, but uses older,
// simpler code to avoid floating point.
func nextSampleNoFP() uintptr {
	// Set first allocation sample size.
	rate := MemProfileRate
	if rate > 0x3fffffff { // make 2*rate not overflow
		rate = 0x3fffffff
	}
	if rate != 0 {
		return uintptr(fastrand() % uint32(2*rate))
	}
	return 0
}

type persistentAlloc struct {
	base *notInHeap
	off  uintptr
}

var globalAlloc struct {
	mutex
	persistentAlloc
}

// persistentChunkSize is the number of bytes we allocate when we grow
// a persistentAlloc.
const persistentChunkSize = 256 << 10

// persistentChunks is a list of all the persistent chunks we have
// allocated. The list is maintained through the first word in the
// persistent chunk. This is updated atomically.
var persistentChunks *notInHeap

// Wrapper around sysAlloc that can allocate small chunks.
// There is no associated free operation.
// Intended for things like function/type/debug-related persistent data.
// If align is 0, uses default align (currently 8).
// The returned memory will be zeroed.
//
// Consider marking persistentalloc'd types go:notinheap.
func persistentalloc(size, align uintptr, sysStat *uint64) unsafe.Pointer {
	var p *notInHeap
	systemstack(func() {
		p = persistentalloc1(size, align, sysStat)
	})
	return unsafe.Pointer(p)
}

// Must run on system stack because stack growth can (re)invoke it.
// See issue 9174.
//go:systemstack
func persistentalloc1(size, align uintptr, sysStat *uint64) *notInHeap {
	const (
		maxBlock = 64 << 10 // VM reservation granularity is 64K on windows
	)

	if size == 0 {
		throw("persistentalloc: size == 0")
	}
	if align != 0 {
		if align&(align-1) != 0 {
			throw("persistentalloc: align is not a power of 2")
		}
		if align > _PageSize {
			throw("persistentalloc: align is too large")
		}
	} else {
		align = 8
	}

	if size >= maxBlock {
		return (*notInHeap)(sysAlloc(size, sysStat))
	}

	mp := acquirem()
	var persistent *persistentAlloc
	if mp != nil && mp.p != 0 {
		persistent = &mp.p.ptr().palloc
	} else {
		lock(&globalAlloc.mutex)
		persistent = &globalAlloc.persistentAlloc
	}
	persistent.off = round(persistent.off, align)
	if persistent.off+size > persistentChunkSize || persistent.base == nil {
		persistent.base = (*notInHeap)(sysAlloc(persistentChunkSize, &memstats.other_sys))
		if persistent.base == nil {
			if persistent == &globalAlloc.persistentAlloc {
				unlock(&globalAlloc.mutex)
			}
			throw("runtime: cannot allocate memory")
		}

		// Add the new chunk to the persistentChunks list.
		for {
			chunks := uintptr(unsafe.Pointer(persistentChunks))
			*(*uintptr)(unsafe.Pointer(persistent.base)) = chunks
			if atomic.Casuintptr((*uintptr)(unsafe.Pointer(&persistentChunks)), chunks, uintptr(unsafe.Pointer(persistent.base))) {
				break
			}
		}
		persistent.off = round(sys.PtrSize, align)
	}
	p := persistent.base.add(persistent.off)
	persistent.off += size
	releasem(mp)
	if persistent == &globalAlloc.persistentAlloc {
		unlock(&globalAlloc.mutex)
	}

	if sysStat != &memstats.other_sys {
		mSysStatInc(sysStat, size)
		mSysStatDec(&memstats.other_sys, size)
	}
	return p
}

// inPersistentAlloc reports whether p points to memory allocated by
// persistentalloc. This must be nosplit because it is called by the
// cgo checker code, which is called by the write barrier code.
//go:nosplit
func inPersistentAlloc(p uintptr) bool {
	chunk := atomic.Loaduintptr((*uintptr)(unsafe.Pointer(&persistentChunks)))
	for chunk != 0 {
		if p >= chunk && p < chunk+persistentChunkSize {
			return true
		}
		chunk = *(*uintptr)(unsafe.Pointer(chunk))
	}
	return false
}

// linearAlloc is a simple linear allocator that pre-reserves a region
// of memory and then maps that region into the Ready state as needed. The
// caller is responsible for locking.
type linearAlloc struct {
	next   uintptr // next free byte
	mapped uintptr // one byte past end of mapped space
	end    uintptr // end of reserved space
}

func (l *linearAlloc) init(base, size uintptr) {
	l.next, l.mapped = base, base
	l.end = base + size
}

func (l *linearAlloc) alloc(size, align uintptr, sysStat *uint64) unsafe.Pointer {
	p := round(l.next, align)
	// 由于linearAlloc仅在32位系统上初始化，在64位系统上next、mapped、end都是0， 因此p=align  p + size > l.end会一直为true
	if p+size > l.end {
		return nil
	}
	l.next = p + size
	if pEnd := round(l.next-1, physPageSize); pEnd > l.mapped {
		// Transition from Reserved to Prepared to Ready.
		sysMap(unsafe.Pointer(l.mapped), pEnd-l.mapped, sysStat)
		sysUsed(unsafe.Pointer(l.mapped), pEnd-l.mapped)
		l.mapped = pEnd
	}
	return unsafe.Pointer(p)
}

// notInHeap is off-heap memory allocated by a lower-level allocator
// like sysAlloc or persistentAlloc.
//
// In general, it's better to use real types marked as go:notinheap,
// but this serves as a generic type for situations where that isn't
// possible (like in the allocators).
//
// TODO: Use this as the return type of sysAlloc, persistentAlloc, etc?
//
//go:notinheap
type notInHeap struct{}

func (p *notInHeap) add(bytes uintptr) *notInHeap {
	return (*notInHeap)(unsafe.Pointer(uintptr(unsafe.Pointer(p)) + bytes))
}
