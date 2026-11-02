package minidb

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// ---------------------------------------------------------------------------
// File layout
//
//	| meta page | free list node | ... pages (B+tree nodes / free list nodes) |
//
// The meta page is the only page that is overwritten atomically in place; it
// holds the root pointer of the latest tree version, the free list pointers and
// the version number:
//
//	| sig | root | npages | head_page | head_seq | tail_page | tail_seq | version |
//	| 16B |  8B  |   8B   |    8B     |    8B    |    8B     |    8B    |   8B    |
//
// An update is a two-phase write: (1) write all new pages and fsync; (2) write
// the meta page and fsync. Because B+tree nodes are copy-on-write, the old
// version stays intact until the meta page flips to the new one.
// ---------------------------------------------------------------------------

const (
	dbSig    = "MiniDB-v1"
	metaSize = 72
)

// KV is a transactional, durable, ordered key-value store.
//
// All methods are safe for concurrent use. Read-only transactions never block
// each other; commits are serialized.
type KV struct {
	Path string
	// internals
	file  *os.File
	mutex sync.Mutex // serializes Begin/Commit/Abort
	tree  btree
	free  freeList
	page  struct {
		flushed uint64            // database size in number of pages
		nappend uint64            // number of pages to be appended
		updates map[uint64][]byte // pending updates, including appended pages
		fresh   map[uint64]struct{}
		reuse   []uint64 // fresh pages freed within the same commit
	}
	failed  bool          // did the last update fail?
	version uint64        // monotonic version number; persisted in the meta page
	ongoing []uint64      // version numbers of concurrent TXs
	history []committedTX // changed keys; for detecting conflicts
}

// ---------------------------------------------------------------------------
// open / close
// ---------------------------------------------------------------------------

// Open opens or creates the database file.
func (kv *KV) Open() (err error) {
	defer catchIO(&err)
	if kv.Path == "" {
		return errors.New("minidb: empty path")
	}
	kv.page.updates = map[uint64][]byte{}
	kv.page.fresh = map[uint64]struct{}{}
	// B+tree callbacks
	kv.tree.get = kv.pageRead  // read a page
	kv.tree.new = kv.pageAlloc // reuse from the free list or append
	kv.tree.del = kv.pageDel   // freed pages go to the free list
	// free list callbacks
	kv.free.get = kv.pageRead
	kv.free.new = kv.pageAppend
	kv.free.set = kv.pageWrite

	f, err := os.OpenFile(kv.Path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return fmt.Errorf("open file: %w", err)
	}
	if err = syncDir(filepath.Dir(kv.Path)); err != nil {
		f.Close()
		return fmt.Errorf("fsync directory: %w", err)
	}
	kv.file = f
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	if err = readRoot(kv, st.Size()); err != nil {
		f.Close()
		return err
	}
	kv.free.maxVer, kv.free.curVer = kv.version, kv.version
	return nil
}

// Close closes the file. Running transactions must be finished first.
func (kv *KV) Close() error {
	kv.mutex.Lock()
	defer kv.mutex.Unlock()
	if kv.file == nil {
		return nil
	}
	err := kv.file.Close()
	kv.file = nil
	return err
}

func readRoot(kv *KV, fileSize int64) error {
	empty := func() {
		// reserve 2 pages: the meta page and a free list node.
		kv.page.flushed = 2
		kv.free.headPage = 1 // the 2nd page
		kv.free.tailPage = 1
		kv.free.headSeq, kv.free.tailSeq = 0, 0
		kv.tree.root = 0
		kv.version = 0
	}
	if fileSize == 0 {
		empty()
		return nil // the meta page will be written in the 1st update
	}
	buf := make([]byte, metaSize)
	if _, err := kv.file.ReadAt(buf, 0); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("read meta page: %w", err)
	}
	if bytes.Equal(buf[:16], make([]byte, 16)) {
		// the 1st commit never completed: logically an empty database.
		empty()
		return nil
	}
	loadMeta(kv, buf)
	if !bytes.Equal(buf[:len(dbSig)], []byte(dbSig)) {
		return errors.New("minidb: bad signature: not a minidb file")
	}
	ok := kv.page.flushed >= 2 &&
		int64(kv.page.flushed)*pageSize <= fileSize &&
		kv.tree.root < kv.page.flushed &&
		kv.free.headPage != 0 && kv.free.headPage < kv.page.flushed &&
		kv.free.tailPage != 0 && kv.free.tailPage < kv.page.flushed &&
		kv.free.headSeq <= kv.free.tailSeq
	if !ok {
		return errors.New("minidb: bad meta page: file is corrupted")
	}
	return nil
}

func saveMeta(kv *KV) []byte {
	var data [metaSize]byte
	copy(data[:16], []byte(dbSig))
	binary.LittleEndian.PutUint64(data[16:], kv.tree.root)
	binary.LittleEndian.PutUint64(data[24:], kv.page.flushed)
	binary.LittleEndian.PutUint64(data[32:], kv.free.headPage)
	binary.LittleEndian.PutUint64(data[40:], kv.free.headSeq)
	binary.LittleEndian.PutUint64(data[48:], kv.free.tailPage)
	binary.LittleEndian.PutUint64(data[56:], kv.free.tailSeq)
	binary.LittleEndian.PutUint64(data[64:], kv.version)
	return data[:]
}

func loadMeta(kv *KV, data []byte) {
	kv.tree.root = binary.LittleEndian.Uint64(data[16:])
	kv.page.flushed = binary.LittleEndian.Uint64(data[24:])
	kv.free.headPage = binary.LittleEndian.Uint64(data[32:])
	kv.free.headSeq = binary.LittleEndian.Uint64(data[40:])
	kv.free.tailPage = binary.LittleEndian.Uint64(data[48:])
	kv.free.tailSeq = binary.LittleEndian.Uint64(data[56:])
	kv.version = binary.LittleEndian.Uint64(data[64:])
}

// ---------------------------------------------------------------------------
// page management
// ---------------------------------------------------------------------------

// read a page straight from the file; safe for concurrent use
func (kv *KV) readPageFile(ptr uint64) []byte {
	buf := make([]byte, pageSize)
	_, err := kv.file.ReadAt(buf, int64(ptr)*pageSize)
	if err != nil && !errors.Is(err, io.EOF) {
		panic(ioPanic{err}) // short reads are zero-filled
	}
	return buf
}

// `btree.get`, `freeList.get`: read a page, consulting pending updates first.
func (kv *KV) pageRead(ptr uint64) []byte {
	if node, ok := kv.page.updates[ptr]; ok {
		return node
	}
	return kv.readPageFile(ptr)
}

// `freeList.new`: append a page.
func (kv *KV) pageAppend(node []byte) uint64 {
	assert(len(node) == pageSize)
	ptr := kv.page.flushed + kv.page.nappend
	kv.page.nappend++
	kv.page.updates[ptr] = node
	return ptr
}

// `freeList.set`: a writable copy of an existing page (in-place update).
func (kv *KV) pageWrite(ptr uint64) []byte {
	if node, ok := kv.page.updates[ptr]; ok {
		return node
	}
	node := make([]byte, pageSize)
	copy(node, kv.readPageFile(ptr))
	kv.page.updates[ptr] = node
	return node
}

// `btree.new`: allocate a page. Pages freed earlier in the same commit are
// reused first (nobody can see them), then the free list, then append.
func (kv *KV) pageAlloc(node []byte) uint64 {
	assert(len(node) == pageSize)
	var ptr uint64
	if n := len(kv.page.reuse); n > 0 {
		ptr = kv.page.reuse[n-1]
		kv.page.reuse = kv.page.reuse[:n-1]
		kv.page.updates[ptr] = node
	} else if p := kv.free.PopHead(); p != 0 {
		ptr = p
		kv.page.updates[ptr] = node
	} else {
		ptr = kv.pageAppend(node)
	}
	kv.page.fresh[ptr] = struct{}{}
	return ptr
}

// `btree.del`: deallocate a page.
func (kv *KV) pageDel(ptr uint64) {
	if _, ok := kv.page.fresh[ptr]; ok {
		// allocated by this very commit: never visible to anyone else.
		delete(kv.page.fresh, ptr)
		delete(kv.page.updates, ptr)
		kv.page.reuse = append(kv.page.reuse, ptr)
		return
	}
	kv.free.PushTail(ptr)
}

func (kv *KV) discardPageState() {
	kv.page.nappend = 0
	kv.page.updates = map[uint64][]byte{}
	kv.page.fresh = map[uint64]struct{}{}
	kv.page.reuse = nil
}

// write all pending pages
func writePages(kv *KV) error {
	// every appended page must exist so the file has no holes at the end
	for i := uint64(0); i < kv.page.nappend; i++ {
		ptr := kv.page.flushed + i
		if _, ok := kv.page.updates[ptr]; !ok {
			kv.page.updates[ptr] = make([]byte, pageSize)
		}
	}
	ptrs := make([]uint64, 0, len(kv.page.updates))
	for ptr := range kv.page.updates {
		ptrs = append(ptrs, ptr)
	}
	sort.Slice(ptrs, func(i, j int) bool { return ptrs[i] < ptrs[j] })
	for _, ptr := range ptrs {
		if _, err := kv.file.WriteAt(kv.page.updates[ptr], int64(ptr)*pageSize); err != nil {
			return fmt.Errorf("write page: %w", err)
		}
	}
	return nil
}

func writeMeta(kv *KV, meta []byte) error {
	if _, err := kv.file.WriteAt(meta, 0); err != nil {
		return fmt.Errorf("write meta page: %w", err)
	}
	return nil
}

// The two-phase update. The caller reverts the in-memory state on error.
func updateFile(kv *KV) error {
	// 1. Write new nodes.
	if err := writePages(kv); err != nil {
		return err
	}
	// 2. `fsync` to enforce the order between 1 and 3.
	if err := kv.file.Sync(); err != nil {
		return fmt.Errorf("fsync: %w", err)
	}
	// the appended pages are part of the database from now on
	kv.page.flushed += kv.page.nappend
	kv.page.nappend = 0
	// 3. Update the meta page. It must be atomic.
	if err := writeMeta(kv, saveMeta(kv)); err != nil {
		return err
	}
	// 4. `fsync` to make everything persistent.
	if err := kv.file.Sync(); err != nil {
		return fmt.Errorf("fsync: %w", err)
	}
	// the state is persistent now
	kv.discardPageState()
	return nil
}

// ---------------------------------------------------------------------------
// transactions
// ---------------------------------------------------------------------------

const (
	flagDeleted = byte(1)
	flagUpdated = byte(2)
)

// KeyRange is an inclusive range of keys: start <= key <= stop.
type KeyRange struct {
	start []byte
	stop  []byte
}

type committedTX struct {
	version uint64
	writes  []KeyRange
}

// KVTX is a transaction with snapshot isolation and optimistic concurrency
// control. Reads see a consistent snapshot plus the transaction's own writes.
// Writes are buffered in memory and applied atomically by Commit, which fails
// with ErrConflict if another transaction changed anything this one depends on.
type KVTX struct {
	db       *KV
	snapshot btree  // a read-only snapshot of the database
	version  uint64 // the version the snapshot is based on
	pending  btree  // captured updates; values are prefixed by a 1-byte flag
	reads    []KeyRange
	done     bool
}

// Begin starts a transaction.
func (kv *KV) Begin(tx *KVTX) {
	kv.mutex.Lock()
	defer kv.mutex.Unlock()
	*tx = KVTX{db: kv, version: kv.version}
	// read-only snapshot: just the tree root and a page reader that never
	// looks at uncommitted pages.
	tx.snapshot.root = kv.tree.root
	tx.snapshot.get = kv.readPageFile
	tx.snapshot.new = func([]byte) uint64 { panic("read-only snapshot") }
	tx.snapshot.del = func(uint64) { panic("read-only snapshot") }
	// in-memory tree to capture updates
	pages := map[uint64][]byte{}
	var freed []uint64
	next := uint64(1)
	tx.pending.get = func(ptr uint64) []byte {
		node, ok := pages[ptr]
		assert(ok)
		return node
	}
	tx.pending.new = func(node []byte) uint64 {
		var ptr uint64
		if n := len(freed); n > 0 {
			ptr, freed = freed[n-1], freed[:n-1]
		} else {
			ptr, next = next, next+1
		}
		pages[ptr] = node
		return ptr
	}
	tx.pending.del = func(ptr uint64) {
		delete(pages, ptr)
		freed = append(freed, ptr)
	}
	kv.ongoing = append(kv.ongoing, tx.version)
}

// Abort discards the transaction.
func (kv *KV) Abort(tx *KVTX) {
	kv.mutex.Lock()
	defer kv.mutex.Unlock()
	if tx.done {
		return
	}
	tx.done = true
	kv.finishTx(tx)
}

// remove the TX from the running list and trim the conflict history. (locked)
func (kv *KV) finishTx(tx *KVTX) {
	for i, v := range kv.ongoing {
		if v == tx.version {
			kv.ongoing = append(kv.ongoing[:i], kv.ongoing[i+1:]...)
			break
		}
	}
	// history entries older than the oldest running TX can't conflict anymore
	oldest, ok := kv.oldestOngoing()
	keep := kv.history[:0]
	for _, h := range kv.history {
		if ok && h.version > oldest {
			keep = append(keep, h)
		}
	}
	kv.history = keep
}

func (kv *KV) oldestOngoing() (uint64, bool) {
	if len(kv.ongoing) == 0 {
		return 0, false
	}
	oldest := kv.ongoing[0]
	for _, v := range kv.ongoing {
		if v < oldest {
			oldest = v
		}
	}
	return oldest, true
}

func rangesOverlap(reads, writes []KeyRange) bool {
	for _, r := range reads {
		for _, w := range writes {
			if bytes.Compare(r.start, w.stop) <= 0 && bytes.Compare(w.start, r.stop) <= 0 {
				return true
			}
		}
	}
	return false
}

// did anybody commit changes to something this TX depends on? (locked)
func (kv *KV) detectConflicts(tx *KVTX) bool {
	for i := len(kv.history) - 1; i >= 0; i-- {
		if kv.history[i].version <= tx.version {
			break // sorted
		}
		if rangesOverlap(tx.reads, kv.history[i].writes) {
			return true
		}
	}
	return false
}

type pendingOp struct {
	key, val []byte
	deleted  bool
}

// Commit applies the transaction atomically and durably.
func (kv *KV) Commit(tx *KVTX) error {
	kv.mutex.Lock()
	defer kv.mutex.Unlock()
	if tx.done {
		return ErrTxDone
	}
	tx.done = true
	defer kv.finishTx(tx)

	if kv.detectConflicts(tx) {
		return ErrConflict
	}
	var ops []pendingOp
	for it := tx.pending.first(); it.Valid(); it.Next() {
		k, v := it.Deref()
		ops = append(ops, pendingOp{key: k, val: v[1:], deleted: v[0] == flagDeleted})
	}
	if len(ops) == 0 {
		return nil // read-only
	}
	if err := kv.apply(tx, ops); err != nil {
		return err
	}
	writes := make([]KeyRange, len(ops))
	for i, op := range ops {
		writes[i] = KeyRange{op.key, op.key}
	}
	kv.history = append(kv.history, committedTX{kv.version, writes})
	return nil
}

// apply the buffered writes to the real tree and persist. (locked)
func (kv *KV) apply(tx *KVTX, ops []pendingOp) (err error) {
	meta := saveMeta(kv) // the last good state, for the rollback
	defer func() {
		r := recover()
		if e, ok := r.(ioPanic); ok {
			err = fmt.Errorf("minidb: io error: %w", e.err)
		} else if r != nil {
			loadMeta(kv, meta)
			kv.discardPageState()
			panic(r)
		}
		if err != nil {
			// revert to the old version; reads keep working.
			loadMeta(kv, meta)
			kv.discardPageState()
			// the on-disk meta page is in an unknown state; rewrite it first
			// thing next time.
			kv.failed = true
		}
	}()

	// ensure the on-disk meta page matches the in-memory one after an error
	if kv.failed {
		if err = writeMeta(kv, meta); err != nil {
			return err
		}
		if err = kv.file.Sync(); err != nil {
			return fmt.Errorf("fsync: %w", err)
		}
		kv.failed = false
	}

	// pages freed by older versions can be reused if no reader can see them:
	// the oldest running reader (not counting this committer) decides.
	kv.free.maxVer = kv.version
	skipped := false
	for _, v := range kv.ongoing {
		if v == tx.version && !skipped {
			skipped = true // this TX itself
			continue
		}
		kv.free.maxVer = min(kv.free.maxVer, v)
	}
	kv.version++
	kv.free.curVer = kv.version

	for _, op := range ops {
		if op.deleted {
			kv.tree.Delete(&DeleteReq{Key: op.key})
		} else {
			kv.tree.Update(&UpdateReq{Key: op.key, Val: op.val, Mode: ModeUpsert})
		}
	}
	// pages allocated and freed within this commit go to the free list now
	for _, ptr := range kv.page.reuse {
		kv.free.PushTail(ptr)
	}
	kv.page.reuse = nil

	return updateFile(kv)
}

// ---------------------------------------------------------------------------
// transactional operations
// ---------------------------------------------------------------------------

func (tx *KVTX) addRead(r KeyRange) {
	tx.reads = append(tx.reads, KeyRange{
		start: append([]byte{}, r.start...),
		stop:  append([]byte{}, r.stop...),
	})
}

// AddRead declares a dependency on a range of keys (used by range queries).
func (tx *KVTX) AddRead(start, stop []byte) { tx.addRead(KeyRange{start, stop}) }

// combines captured updates with the snapshot
func (tx *KVTX) get(key []byte) ([]byte, bool) {
	val, ok := tx.pending.Get(key)
	switch {
	case ok && val[0] == flagUpdated: // updated in this TX
		return val[1:], true
	case ok && val[0] == flagDeleted: // deleted in this TX
		return nil, false
	default: // read from the snapshot
		return tx.snapshot.Get(key)
	}
}

// Get is a point query. The result must not be modified.
func (tx *KVTX) Get(key []byte) (val []byte, ok bool, err error) {
	defer catchIO(&err)
	if tx.done {
		return nil, false, ErrTxDone
	}
	tx.addRead(KeyRange{key, key})
	val, ok = tx.get(key)
	return val, ok, nil
}

// Update inserts or updates a key. See UpdateReq for the in/out fields.
// It returns whether the key was changed.
func (tx *KVTX) Update(req *UpdateReq) (changed bool, err error) {
	defer catchIO(&err)
	if tx.done {
		return false, ErrTxDone
	}
	if len(req.Key) == 0 || len(req.Key) > maxKeySize {
		return false, ErrKeySize
	}
	if len(req.Val) > maxValSize {
		return false, ErrValSize
	}
	tx.addRead(KeyRange{req.Key, req.Key}) // the result depends on the old state
	old, exists := tx.get(req.Key)
	req.Added, req.Updated, req.Old = false, false, nil
	if exists {
		req.Old = append([]byte{}, old...)
	}
	switch {
	case req.Mode == ModeInsertOnly && exists:
		return false, nil
	case req.Mode == ModeUpdateOnly && !exists:
		return false, nil
	case exists && bytes.Equal(req.Old, req.Val):
		return false, nil // no change
	}
	val := append([]byte{flagUpdated}, req.Val...)
	tx.pending.Update(&UpdateReq{Key: req.Key, Val: val, Mode: ModeUpsert})
	req.Added, req.Updated = !exists, true
	return true, nil
}

// Del deletes a key. Returns whether the key was there.
func (tx *KVTX) Del(req *DeleteReq) (deleted bool, err error) {
	defer catchIO(&err)
	if tx.done {
		return false, ErrTxDone
	}
	if len(req.Key) == 0 || len(req.Key) > maxKeySize {
		return false, ErrKeySize
	}
	tx.addRead(KeyRange{req.Key, req.Key})
	old, exists := tx.get(req.Key)
	req.Old = nil
	if !exists {
		return false, nil
	}
	req.Old = append([]byte{}, old...)
	tx.pending.Update(&UpdateReq{Key: req.Key, Val: []byte{flagDeleted}, Mode: ModeUpsert})
	return true, nil
}

// Set is shorthand for an upsert.
func (tx *KVTX) Set(key, val []byte) error {
	_, err := tx.Update(&UpdateReq{Key: key, Val: val, Mode: ModeUpsert})
	return err
}

// Delete is shorthand for Del.
func (tx *KVTX) Delete(key []byte) (bool, error) {
	return tx.Del(&DeleteReq{Key: key})
}

// ---------------------------------------------------------------------------
// range queries over (pending updates + snapshot)
// ---------------------------------------------------------------------------

// KVIter iterates over the transaction's view of the data. The direction is
// decided by the comparison operator given to Seek: CmpGE/CmpGT iterate in
// ascending order, CmpLE/CmpLT in descending order. Next always moves forward
// in that direction.
type KVIter struct {
	tx   *KVTX
	top  *biter // KVTX.pending
	bot  *biter // KVTX.snapshot
	dir  int
	ridx int // index of this iterator's dependency in tx.reads
}

// Seek returns an iterator positioned at the closest key satisfying
// `key' cmp key`. Keys visited by the iterator become dependencies of the
// transaction.
func (tx *KVTX) Seek(key []byte, cmp int) (it *KVIter, err error) {
	defer catchIO(&err)
	if tx.done {
		return nil, ErrTxDone
	}
	it = &KVIter{tx: tx, top: tx.pending.Seek(key, cmp), bot: tx.snapshot.Seek(key, cmp)}
	if cmp > 0 {
		it.dir = +1
	} else {
		it.dir = -1
	}
	it.ridx = len(tx.reads)
	tx.addRead(KeyRange{key, key})
	it.skipDeleted()
	it.track()
	return it, nil
}

// which iterator holds the current item? 1: top, 2: bot, 0: none
func (it *KVIter) pick() int {
	tv, bv := it.top.Valid(), it.bot.Valid()
	switch {
	case !tv && !bv:
		return 0
	case tv && !bv:
		return 1
	case !tv && bv:
		return 2
	}
	tk, _ := it.top.Deref()
	bk, _ := it.bot.Deref()
	c := bytes.Compare(tk, bk)
	if c == 0 {
		return 1 // pending updates shadow the snapshot
	}
	if (it.dir > 0) == (c < 0) {
		return 1
	}
	return 2
}

func (it *KVIter) move(b *biter) {
	if it.dir > 0 {
		b.Next()
	} else {
		b.Prev()
	}
}

func (it *KVIter) advance() {
	switch it.pick() {
	case 1:
		if it.bot.Valid() {
			tk, _ := it.top.Deref()
			bk, _ := it.bot.Deref()
			if bytes.Equal(tk, bk) {
				it.move(it.bot) // shadowed
			}
		}
		it.move(it.top)
	case 2:
		it.move(it.bot)
	}
}

func (it *KVIter) skipDeleted() {
	for it.pick() == 1 {
		_, v := it.top.Deref()
		if v[0] != flagDeleted {
			return
		}
		it.advance()
	}
}

// extend the recorded dependency to cover the current position
func (it *KVIter) track() {
	if w := it.pick(); w != 0 {
		var k []byte
		if w == 1 {
			k, _ = it.top.Deref()
		} else {
			k, _ = it.bot.Deref()
		}
		r := &it.tx.reads[it.ridx]
		if bytes.Compare(k, r.start) < 0 {
			r.start = append([]byte{}, k...)
		}
		if bytes.Compare(k, r.stop) > 0 {
			r.stop = append([]byte{}, k...)
		}
	}
}

// Valid reports whether the iterator points to a KV.
func (it *KVIter) Valid() bool { return it.pick() != 0 }

// Next moves forward in the direction of the scan.
func (it *KVIter) Next() {
	it.advance()
	it.skipDeleted()
	it.track()
}

// Deref returns the current KV pair. Results must not be modified.
func (it *KVIter) Deref() (key, val []byte) {
	switch it.pick() {
	case 1:
		k, v := it.top.Deref()
		return k, v[1:]
	case 2:
		return it.bot.Deref()
	}
	panic("invalid iterator")
}

// ---------------------------------------------------------------------------
// convenience API: run a function in a transaction
// ---------------------------------------------------------------------------

const maxRetries = 20

// View runs a read-only function on a consistent snapshot.
func (kv *KV) View(fn func(tx *KVTX) error) error {
	var tx KVTX
	kv.Begin(&tx)
	defer kv.Abort(&tx)
	return fn(&tx)
}

// Update runs fn in a read-write transaction and commits it. If the commit
// conflicts with a concurrent transaction, fn is run again, so it must be
// safe to repeat.
func (kv *KV) Update(fn func(tx *KVTX) error) error {
	for i := 0; ; i++ {
		var tx KVTX
		kv.Begin(&tx)
		if err := fn(&tx); err != nil {
			kv.Abort(&tx)
			return err
		}
		err := kv.Commit(&tx)
		if errors.Is(err, ErrConflict) && i < maxRetries {
			continue
		}
		return err
	}
}

// Get reads a single key.
func (kv *KV) Get(key []byte) (val []byte, ok bool, err error) {
	err = kv.View(func(tx *KVTX) error {
		v, found, err := tx.Get(key)
		if found {
			val = append([]byte{}, v...)
		}
		ok = found
		return err
	})
	return
}

// Set writes a single key durably.
func (kv *KV) Set(key, val []byte) error {
	return kv.Update(func(tx *KVTX) error { return tx.Set(key, val) })
}

// Del removes a single key durably. Returns whether the key was there.
func (kv *KV) Del(key []byte) (deleted bool, err error) {
	err = kv.Update(func(tx *KVTX) error {
		var e error
		deleted, e = tx.Delete(key)
		return e
	})
	return
}
