package minidb

import (
	"bytes"
	"encoding/binary"
)

// ---------------------------------------------------------------------------
// B+tree node format (all nodes are exactly one page)
//
//	| type | nkeys | pointers   | offsets    | key-values | unused |
//	|  2B  |  2B   | nkeys * 8B | nkeys * 2B |    ...     |        |
//
// each KV:  | klen | vlen | key | val |
//	         |  2B  |  2B  | ... | ... |
// ---------------------------------------------------------------------------

const (
	nodeHeader = 4
	pageSize   = 4096
	maxKeySize = 1000
	maxValSize = 3000
)

const (
	nodeInternal = 1 // internal nodes: keys + child pointers, no values
	nodeLeaf     = 2 // leaf nodes: keys + values
)

func init() {
	node1max := nodeHeader + 8 + 2 + 4 + maxKeySize + maxValSize + 1 // +1: tx flag byte
	assert(node1max <= pageSize)
}

// bnode is a chunk of bytes interpreted by the format above. It can be dumped
// to disk as is.
type bnode []byte

// btree is a copy-on-write B+tree. It knows nothing about disks: pages are
// managed through three callbacks.
type btree struct {
	root uint64              // page number of the root; 0 = empty tree
	get  func(uint64) []byte // dereference a pointer
	new  func([]byte) uint64 // allocate a new page
	del  func(uint64)        // deallocate a page
}

// --- header ---

func (n bnode) btype() uint16 { return binary.LittleEndian.Uint16(n[0:2]) }
func (n bnode) nkeys() uint16 { return binary.LittleEndian.Uint16(n[2:4]) }
func (n bnode) setHeader(btype, nkeys uint16) {
	binary.LittleEndian.PutUint16(n[0:2], btype)
	binary.LittleEndian.PutUint16(n[2:4], nkeys)
}

// --- child pointers ---

func (n bnode) getPtr(idx uint16) uint64 {
	assert(idx < n.nkeys())
	pos := nodeHeader + 8*idx
	return binary.LittleEndian.Uint64(n[pos:])
}
func (n bnode) setPtr(idx uint16, val uint64) {
	assert(idx < n.nkeys())
	pos := nodeHeader + 8*idx
	binary.LittleEndian.PutUint64(n[pos:], val)
}

// --- offsets: end of the idx-th KV relative to the start of the 1st KV ---

func offsetPos(n bnode, idx uint16) uint16 {
	assert(1 <= idx && idx <= n.nkeys())
	return nodeHeader + 8*n.nkeys() + 2*(idx-1)
}
func (n bnode) getOffset(idx uint16) uint16 {
	if idx == 0 {
		return 0
	}
	return binary.LittleEndian.Uint16(n[offsetPos(n, idx):])
}
func (n bnode) setOffset(idx uint16, offset uint16) {
	binary.LittleEndian.PutUint16(n[offsetPos(n, idx):], offset)
}

// --- key-values ---

func (n bnode) kvPos(idx uint16) uint16 {
	assert(idx <= n.nkeys())
	return nodeHeader + 8*n.nkeys() + 2*n.nkeys() + n.getOffset(idx)
}
func (n bnode) getKey(idx uint16) []byte {
	assert(idx < n.nkeys())
	pos := n.kvPos(idx)
	klen := binary.LittleEndian.Uint16(n[pos:])
	return n[pos+4:][:klen]
}
func (n bnode) getVal(idx uint16) []byte {
	assert(idx < n.nkeys())
	pos := n.kvPos(idx)
	klen := binary.LittleEndian.Uint16(n[pos+0:])
	vlen := binary.LittleEndian.Uint16(n[pos+2:])
	return n[pos+4+klen:][:vlen]
}

// node size in bytes
func (n bnode) nbytes() uint16 { return n.kvPos(n.nkeys()) }

// returns the last position whose key is <= the given key.
// The first key of every node is a copy of the parent's separator (or the
// empty sentinel key), so it is always <= key and position 0 is the fallback.
func nodeLookupLE(node bnode, key []byte) uint16 {
	lo, hi := uint16(1), node.nkeys()
	found := uint16(0)
	for lo < hi {
		mid := (lo + hi) / 2
		if bytes.Compare(node.getKey(mid), key) <= 0 {
			found = mid
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return found
}

// ---------------------------------------------------------------------------
// node updates (copy-on-write: always build a new node)
// ---------------------------------------------------------------------------

// copy a KV into the position. Must be called in order (relies on offsets).
func nodeAppendKV(dst bnode, idx uint16, ptr uint64, key, val []byte) {
	dst.setPtr(idx, ptr)
	pos := dst.kvPos(idx)
	binary.LittleEndian.PutUint16(dst[pos+0:], uint16(len(key)))
	binary.LittleEndian.PutUint16(dst[pos+2:], uint16(len(val)))
	copy(dst[pos+4:], key)
	copy(dst[pos+4+uint16(len(key)):], val)
	dst.setOffset(idx+1, dst.getOffset(idx)+4+uint16(len(key)+len(val)))
}

// copy n KVs from src[srcOld:] into dst[dstNew:]
func nodeAppendRange(dst, src bnode, dstNew, srcOld, n uint16) {
	assert(srcOld+n <= src.nkeys())
	assert(dstNew+n <= dst.nkeys())
	if n == 0 {
		return
	}
	for i := uint16(0); i < n; i++ {
		dst.setPtr(dstNew+i, src.getPtr(srcOld+i))
	}
	dstBegin := dst.getOffset(dstNew)
	srcBegin := src.getOffset(srcOld)
	for i := uint16(1); i <= n; i++ {
		dst.setOffset(dstNew+i, dstBegin+src.getOffset(srcOld+i)-srcBegin)
	}
	begin := src.kvPos(srcOld)
	end := src.kvPos(srcOld + n)
	copy(dst[dst.kvPos(dstNew):], src[begin:end])
}

// add a new key to a leaf node
func leafInsert(dst, src bnode, idx uint16, key, val []byte) {
	dst.setHeader(nodeLeaf, src.nkeys()+1)
	nodeAppendRange(dst, src, 0, 0, idx)
	nodeAppendKV(dst, idx, 0, key, val)
	nodeAppendRange(dst, src, idx+1, idx, src.nkeys()-idx)
}

// update an existing key of a leaf node
func leafUpdate(dst, src bnode, idx uint16, key, val []byte) {
	dst.setHeader(nodeLeaf, src.nkeys())
	nodeAppendRange(dst, src, 0, 0, idx)
	nodeAppendKV(dst, idx, 0, key, val)
	nodeAppendRange(dst, src, idx+1, idx+1, src.nkeys()-(idx+1))
}

// remove a key from a leaf node
func leafDelete(dst, src bnode, idx uint16) {
	dst.setHeader(nodeLeaf, src.nkeys()-1)
	nodeAppendRange(dst, src, 0, 0, idx)
	nodeAppendRange(dst, src, idx, idx+1, src.nkeys()-(idx+1))
}

// merge 2 nodes into 1
func nodeMerge(dst, left, right bnode) {
	assert(left.btype() == right.btype())
	dst.setHeader(left.btype(), left.nkeys()+right.nkeys())
	nodeAppendRange(dst, left, 0, 0, left.nkeys())
	nodeAppendRange(dst, right, left.nkeys(), 0, right.nkeys())
}

// replace 2 adjacent links with 1
func nodeReplace2Kid(dst, src bnode, idx uint16, ptr uint64, key []byte) {
	dst.setHeader(nodeInternal, src.nkeys()-1)
	nodeAppendRange(dst, src, 0, 0, idx)
	nodeAppendKV(dst, idx, ptr, key, nil)
	nodeAppendRange(dst, src, idx+1, idx+2, src.nkeys()-(idx+2))
}

// replace a link with one or multiple links
func nodeReplaceKidN(tree *btree, dst, src bnode, idx uint16, kids ...bnode) {
	inc := uint16(len(kids))
	dst.setHeader(nodeInternal, src.nkeys()+inc-1)
	nodeAppendRange(dst, src, 0, 0, idx)
	for i, node := range kids {
		nodeAppendKV(dst, idx+uint16(i), tree.new(node), node.getKey(0), nil)
	}
	nodeAppendRange(dst, src, idx+inc, idx+1, src.nkeys()-(idx+1))
}

// split an oversized node into 2 so that the 2nd node always fits on a page
func nodeSplit2(left, right, old bnode) {
	assert(old.nkeys() >= 2)
	nleft := old.nkeys() / 2
	leftBytes := func() uint16 {
		return nodeHeader + 10*nleft + old.getOffset(nleft)
	}
	for leftBytes() > pageSize {
		nleft--
	}
	assert(nleft >= 1)
	rightBytes := func() uint16 {
		return old.nbytes() - leftBytes() + nodeHeader
	}
	for rightBytes() > pageSize {
		nleft++
	}
	assert(nleft < old.nkeys())
	nright := old.nkeys() - nleft

	left.setHeader(old.btype(), nleft)
	right.setHeader(old.btype(), nright)
	nodeAppendRange(left, old, 0, 0, nleft)
	nodeAppendRange(right, old, 0, nleft, nright)
	assert(right.nbytes() <= pageSize)
}

// split a node if it's too big. the results are 1~3 nodes.
func nodeSplit3(old bnode) (uint16, [3]bnode) {
	if old.nbytes() <= pageSize {
		old = old[:pageSize]
		return 1, [3]bnode{old}
	}
	left := bnode(make([]byte, 2*pageSize)) // might be split again
	right := bnode(make([]byte, pageSize))
	nodeSplit2(left, right, old)
	if left.nbytes() <= pageSize {
		left = left[:pageSize]
		return 2, [3]bnode{left, right}
	}
	leftleft := bnode(make([]byte, pageSize))
	middle := bnode(make([]byte, pageSize))
	nodeSplit2(leftleft, middle, left)
	assert(leftleft.nbytes() <= pageSize)
	return 3, [3]bnode{leftleft, middle, right}
}

// ---------------------------------------------------------------------------
// insertion / update
// ---------------------------------------------------------------------------

// update modes
const (
	ModeUpsert     = 0 // insert or replace
	ModeUpdateOnly = 1 // update existing keys only
	ModeInsertOnly = 2 // add new keys only
)

// UpdateReq is both the input and the output of a tree update.
type UpdateReq struct {
	tree *btree
	// out
	Added   bool   // added a new key
	Updated bool   // added a new key or an old key was changed
	Old     []byte // the value before the update (nil if the key was absent)
	// in
	Key  []byte
	Val  []byte
	Mode int
}

// DeleteReq is both the input and the output of a tree deletion.
type DeleteReq struct {
	tree *btree
	// in
	Key []byte
	// out
	Old []byte
}

// insert a KV into a node; the result might be oversized and will be split by
// the caller. Returns nil if nothing changed.
func treeInsert(req *UpdateReq, node bnode) bnode {
	dst := bnode(make([]byte, 2*pageSize))
	idx := nodeLookupLE(node, req.Key)
	switch node.btype() {
	case nodeLeaf:
		if bytes.Equal(req.Key, node.getKey(idx)) {
			req.Old = append([]byte{}, node.getVal(idx)...)
			if req.Mode == ModeInsertOnly || bytes.Equal(req.Old, req.Val) {
				return nil
			}
			leafUpdate(dst, node, idx, req.Key, req.Val)
			req.Updated = true
		} else {
			if req.Mode == ModeUpdateOnly {
				return nil
			}
			leafInsert(dst, node, idx+1, req.Key, req.Val)
			req.Added, req.Updated = true, true
		}
	case nodeInternal:
		if !nodeInsert(req, dst, node, idx) {
			return nil
		}
	default:
		panic("bad node")
	}
	return dst
}

// KV insertion into an internal node
func nodeInsert(req *UpdateReq, dst, node bnode, idx uint16) bool {
	kptr := node.getPtr(idx)
	knode := treeInsert(req, bnode(req.tree.get(kptr)))
	if len(knode) == 0 {
		return false
	}
	nsplit, split := nodeSplit3(knode)
	req.tree.del(kptr)
	nodeReplaceKidN(req.tree, dst, node, idx, split[:nsplit]...)
	return true
}

// Update inserts or updates a key depending on req.Mode. Returns whether the
// tree changed.
func (tree *btree) Update(req *UpdateReq) bool {
	assert(len(req.Key) != 0)
	assert(len(req.Key) <= maxKeySize)
	assert(len(req.Val) <= maxValSize)
	req.tree = tree
	req.Added, req.Updated, req.Old = false, false, nil

	if tree.root == 0 {
		if req.Mode == ModeUpdateOnly {
			return false
		}
		root := bnode(make([]byte, pageSize))
		root.setHeader(nodeLeaf, 2)
		// a dummy (empty) key: the lowest possible key, so that a lookup can
		// always find a containing position.
		nodeAppendKV(root, 0, 0, nil, nil)
		nodeAppendKV(root, 1, 0, req.Key, req.Val)
		tree.root = tree.new(root)
		req.Added, req.Updated = true, true
		return true
	}

	node := treeInsert(req, bnode(tree.get(tree.root)))
	if len(node) == 0 {
		return false
	}
	nsplit, split := nodeSplit3(node)
	tree.del(tree.root)
	if nsplit > 1 {
		// the root was split, add a new level.
		root := bnode(make([]byte, pageSize))
		root.setHeader(nodeInternal, nsplit)
		for i, knode := range split[:nsplit] {
			ptr, key := tree.new(knode), knode.getKey(0)
			nodeAppendKV(root, uint16(i), ptr, key, nil)
		}
		tree.root = tree.new(root)
	} else {
		tree.root = tree.new(split[0])
	}
	return true
}

// Insert is a convenience wrapper (upsert).
func (tree *btree) Insert(key, val []byte) {
	tree.Update(&UpdateReq{Key: key, Val: val, Mode: ModeUpsert})
}

// ---------------------------------------------------------------------------
// deletion
// ---------------------------------------------------------------------------

// should the updated kid be merged with a sibling?
func shouldMerge(tree *btree, node bnode, idx uint16, updated bnode) (int, bnode) {
	if updated.nbytes() > pageSize/4 {
		return 0, nil
	}
	if idx > 0 {
		sibling := bnode(tree.get(node.getPtr(idx - 1)))
		merged := sibling.nbytes() + updated.nbytes() - nodeHeader
		if merged <= pageSize {
			return -1, sibling // left
		}
	}
	if idx+1 < node.nkeys() {
		sibling := bnode(tree.get(node.getPtr(idx + 1)))
		merged := sibling.nbytes() + updated.nbytes() - nodeHeader
		if merged <= pageSize {
			return +1, sibling // right
		}
	}
	return 0, nil
}

// delete a key from the tree; returns nil if the key was not found
func treeDelete(tree *btree, node bnode, key []byte) bnode {
	idx := nodeLookupLE(node, key)
	switch node.btype() {
	case nodeLeaf:
		if !bytes.Equal(key, node.getKey(idx)) {
			return nil
		}
		dst := bnode(make([]byte, pageSize))
		leafDelete(dst, node, idx)
		return dst
	case nodeInternal:
		return nodeDelete(tree, node, idx, key)
	default:
		panic("bad node")
	}
}

// delete a key from an internal node; part of treeDelete()
func nodeDelete(tree *btree, node bnode, idx uint16, key []byte) bnode {
	kptr := node.getPtr(idx)
	updated := treeDelete(tree, bnode(tree.get(kptr)), key)
	if len(updated) == 0 {
		return nil // not found
	}
	tree.del(kptr)

	// the result may be oversized: deleting a kid's first key changes the
	// separator key in this node, which can be longer than the old one.
	dst := bnode(make([]byte, 2*pageSize))
	mergeDir, sibling := shouldMerge(tree, node, idx, updated)
	switch {
	case mergeDir < 0: // left
		merged := bnode(make([]byte, pageSize))
		nodeMerge(merged, sibling, updated)
		tree.del(node.getPtr(idx - 1))
		nodeReplace2Kid(dst, node, idx-1, tree.new(merged), merged.getKey(0))
	case mergeDir > 0: // right
		merged := bnode(make([]byte, pageSize))
		nodeMerge(merged, updated, sibling)
		tree.del(node.getPtr(idx + 1))
		nodeReplace2Kid(dst, node, idx, tree.new(merged), merged.getKey(0))
	case mergeDir == 0 && updated.nkeys() == 0:
		assert(node.nkeys() == 1 && idx == 0) // 1 empty child but no sibling
		dst.setHeader(nodeInternal, 0)        // the parent becomes empty too
	case mergeDir == 0 && updated.nkeys() > 0: // no merge
		nsplit, split := nodeSplit3(updated)
		nodeReplaceKidN(tree, dst, node, idx, split[:nsplit]...)
	}
	return dst
}

// Delete removes a key. Returns whether the key was there.
func (tree *btree) Delete(req *DeleteReq) bool {
	assert(len(req.Key) != 0)
	assert(len(req.Key) <= maxKeySize)
	req.tree = tree
	req.Old = nil
	if tree.root == 0 {
		return false
	}
	old, ok := tree.Get(req.Key)
	if !ok {
		return false
	}
	req.Old = append([]byte{}, old...)

	updated := treeDelete(tree, bnode(tree.get(tree.root)), req.Key)
	if len(updated) == 0 {
		return false
	}
	nsplit, split := nodeSplit3(updated)
	tree.del(tree.root)
	switch {
	case nsplit > 1:
		// the root was split, add a new level.
		root := bnode(make([]byte, pageSize))
		root.setHeader(nodeInternal, nsplit)
		for i, knode := range split[:nsplit] {
			nodeAppendKV(root, uint16(i), tree.new(knode), knode.getKey(0), nil)
		}
		tree.root = tree.new(root)
	case split[0].btype() == nodeInternal && split[0].nkeys() == 1:
		// remove a level
		tree.root = split[0].getPtr(0)
	default:
		tree.root = tree.new(split[0])
	}
	return true
}

// ---------------------------------------------------------------------------
// point query
// ---------------------------------------------------------------------------

// Get returns the value of a key. The returned slice must not be modified.
func (tree *btree) Get(key []byte) ([]byte, bool) {
	if tree.root == 0 || len(key) == 0 {
		return nil, false
	}
	node := bnode(tree.get(tree.root))
	for {
		idx := nodeLookupLE(node, key)
		switch node.btype() {
		case nodeLeaf:
			if bytes.Equal(node.getKey(idx), key) {
				return node.getVal(idx), true
			}
			return nil, false
		case nodeInternal:
			node = bnode(tree.get(node.getPtr(idx)))
		default:
			panic("bad node")
		}
	}
}

// ---------------------------------------------------------------------------
// iterator
// ---------------------------------------------------------------------------

// comparison operators for seeks and ranges
const (
	CmpGE = +3 // >=
	CmpGT = +2 // >
	CmpLT = -2 // <
	CmpLE = -3 // <=
)

func cmpOK(key []byte, cmp int, ref []byte) bool {
	r := bytes.Compare(key, ref)
	switch cmp {
	case CmpGE:
		return r >= 0
	case CmpGT:
		return r > 0
	case CmpLT:
		return r < 0
	case CmpLE:
		return r <= 0
	default:
		panic("bad comparison operator")
	}
}

// biter is a position in the tree. Since there are no parent pointers, it
// keeps the whole path from the root to a leaf.
type biter struct {
	tree *btree
	path []bnode  // from root to leaf
	pos  []uint16 // indexes into nodes
}

// find the closest position that is less or equal to the input key
func (tree *btree) SeekLE(key []byte) *biter {
	iter := &biter{tree: tree}
	for ptr := tree.root; ptr != 0; {
		node := bnode(tree.get(ptr))
		idx := nodeLookupLE(node, key)
		iter.path = append(iter.path, node)
		iter.pos = append(iter.pos, idx)
		if node.btype() == nodeInternal {
			ptr = node.getPtr(idx)
		} else {
			ptr = 0
		}
	}
	return iter
}

// Seek finds the closest position satisfying `position cmp key`.
func (tree *btree) Seek(key []byte, cmp int) *biter {
	iter := tree.SeekLE(key)
	if !iter.inRange() {
		return iter
	}
	cur, _ := iter.raw()
	switch cmp {
	case CmpLE:
		// SeekLE already did it
	case CmpLT:
		if bytes.Equal(cur, key) {
			iter.Prev()
		}
	case CmpGE:
		if bytes.Compare(cur, key) < 0 {
			iter.Next()
		}
	case CmpGT:
		iter.Next()
	default:
		panic("bad comparison operator")
	}
	return iter
}

// position of the 1st real key (skipping the sentinel)
func (tree *btree) first() *biter {
	iter := tree.SeekLE(nil)
	iter.Next()
	return iter
}

func (iter *biter) inRange() bool {
	if len(iter.path) == 0 {
		return false
	}
	last := len(iter.path) - 1
	return iter.pos[last] < iter.path[last].nkeys()
}

func (iter *biter) raw() ([]byte, []byte) {
	last := len(iter.path) - 1
	node := iter.path[last]
	return node.getKey(iter.pos[last]), node.getVal(iter.pos[last])
}

// Valid reports whether the iterator points to a real KV (not the sentinel,
// and not past either end).
func (iter *biter) Valid() bool {
	if !iter.inRange() {
		return false
	}
	k, _ := iter.raw()
	return len(k) > 0
}

// Deref returns the current KV pair.
func (iter *biter) Deref() ([]byte, []byte) {
	assert(iter.Valid())
	return iter.raw()
}

func (iter *biter) Next() {
	if len(iter.path) > 0 {
		iterNext(iter, len(iter.path)-1)
	}
}

func (iter *biter) Prev() {
	if len(iter.path) > 0 {
		iterPrev(iter, len(iter.path)-1)
	}
}

// moving is like carrying when incrementing a number digit by digit.
func iterNext(iter *biter, level int) bool {
	if iter.pos[level]+1 < iter.path[level].nkeys() {
		iter.pos[level]++ // move within this node
	} else if level > 0 && iterNext(iter, level-1) {
		// moved to a sibling node
	} else {
		// past the last key
		last := len(iter.pos) - 1
		iter.pos[last] = iter.path[last].nkeys()
		return false
	}
	if level+1 < len(iter.pos) { // update the child node
		node := iter.path[level]
		kid := bnode(iter.tree.get(node.getPtr(iter.pos[level])))
		iter.path[level+1] = kid
		iter.pos[level+1] = 0
	}
	return true
}

func iterPrev(iter *biter, level int) bool {
	if iter.pos[level] > 0 {
		iter.pos[level]--
	} else if level > 0 && iterPrev(iter, level-1) {
		// moved to a sibling node
	} else {
		return false // before the first key; position unchanged
	}
	if level+1 < len(iter.pos) {
		node := iter.path[level]
		kid := bnode(iter.tree.get(node.getPtr(iter.pos[level])))
		iter.path[level+1] = kid
		iter.pos[level+1] = kid.nkeys() - 1
	}
	return true
}
