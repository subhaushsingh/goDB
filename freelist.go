package minidb

import "encoding/binary"

// A page-based, self-managing, unrolled linked list of unused pages.
//
// Node format:
//
//	| next | (pointer, version) * n | unused |
//	|  8B  |        n * 16B         |   ...  |
//
// Items are appended to the tail node and consumed from the head. A page freed
// by the commit that produced version V is tagged with V; it can only be reused
// once every running reader is at version >= V (see maxVer).
type lnode []byte

const (
	flHeader   = 8
	flItemSize = 16
	flCap      = (pageSize - flHeader) / flItemSize
)

func (n lnode) getNext() uint64     { return binary.LittleEndian.Uint64(n[0:]) }
func (n lnode) setNext(next uint64) { binary.LittleEndian.PutUint64(n[0:], next) }
func (n lnode) getItem(idx int) (ptr, ver uint64) {
	pos := flHeader + flItemSize*idx
	return binary.LittleEndian.Uint64(n[pos:]), binary.LittleEndian.Uint64(n[pos+8:])
}
func (n lnode) setItem(idx int, ptr, ver uint64) {
	pos := flHeader + flItemSize*idx
	binary.LittleEndian.PutUint64(n[pos:], ptr)
	binary.LittleEndian.PutUint64(n[pos+8:], ver)
}

type freeList struct {
	// callbacks for managing on-disk pages
	get func(uint64) []byte // read a page
	new func([]byte) uint64 // append a new page
	set func(uint64) []byte // update an existing page in place
	// persisted data in the meta page
	headPage uint64 // pointer to the list head node
	headSeq  uint64 // monotonic sequence number to index into the list head
	tailPage uint64
	tailSeq  uint64
	// in-memory states
	maxVer uint64 // the oldest reader version: newer items can't be reused
	curVer uint64 // version number of the commit in progress
}

// a monotonically increasing sequence number maps to a wrapped-around index
func seq2idx(seq uint64) int { return int(seq % flCap) }

// remove 1 item from the head node, and remove the head node if empty.
func flPop(fl *freeList) (ptr uint64, head uint64) {
	if fl.headSeq == fl.tailSeq {
		return 0, 0 // empty
	}
	node := lnode(fl.get(fl.headPage))
	ptr, ver := node.getItem(seq2idx(fl.headSeq))
	if ver > fl.maxVer {
		return 0, 0 // still visible to some reader
	}
	fl.headSeq++
	// move to the next one if the head node is empty
	if seq2idx(fl.headSeq) == 0 {
		head, fl.headPage = fl.headPage, node.getNext()
		assert(fl.headPage != 0)
	}
	return
}

// PopHead gets 1 item from the list head. Returns 0 on failure.
func (fl *freeList) PopHead() uint64 {
	ptr, head := flPop(fl)
	if head != 0 { // the empty head node is recycled
		fl.PushTail(head)
	}
	return ptr
}

// PushTail adds 1 item to the tail.
func (fl *freeList) PushTail(ptr uint64) {
	// add it to the tail node
	lnode(fl.set(fl.tailPage)).setItem(seq2idx(fl.tailSeq), ptr, fl.curVer)
	fl.tailSeq++
	// add a new tail node if it's full (the list is never empty)
	if seq2idx(fl.tailSeq) == 0 {
		// try to reuse from the list head. Not when that would exhaust the
		// head node while it is also the tail node: its `next` link doesn't
		// exist yet.
		var next, head uint64
		if fl.headPage != fl.tailPage || seq2idx(fl.headSeq+1) != 0 {
			next, head = flPop(fl) // may remove the head node
		}
		if next == 0 {
			// or allocate a new node by appending
			next = fl.new(make([]byte, pageSize))
		}
		// link to the new tail node
		lnode(fl.set(fl.tailPage)).setNext(next)
		fl.tailPage = next
		// also add the head node if it's removed
		if head != 0 {
			lnode(fl.set(fl.tailPage)).setItem(0, head, fl.curVer)
			fl.tailSeq++
		}
	}
}
