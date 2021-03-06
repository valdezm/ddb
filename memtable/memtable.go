//    Copyright 2018 Google Inc.
//
//    Licensed under the Apache License, Version 2.0 (the "License");
//    you may not use this file except in compliance with the License.
//    You may obtain a copy of the License at
//
//        http://www.apache.org/licenses/LICENSE-2.0
//
//    Unless required by applicable law or agreed to in writing, software
//    distributed under the License is distributed on an "AS IS" BASIS,
//    WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
//    See the License for the specific language governing permissions and
//    limitations under the License.

package memtable

import (
	"math"
	"math/bits"
	"math/rand"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/golang/glog"
)

const (
	maxLevel = 16
)

// Memtable is an in-memory sorted table of (key, timestamp) -> values.
// keys are sorted ascending, timestamps descending.
type Memtable struct {
	head *node
	rnd  *rand.Rand

	// seqNoUpper is the largest log sequence number that has been applied.
	seqNoUpper int64

	size int64
	mu   sync.Mutex
}

type node struct {
	key       string
	timestamp int64
	value     []byte
	next      []unsafe.Pointer // actual type is *node
}

func (n *node) atomicStoreNext(l int, x *node) {
	atomic.StorePointer(&n.next[l], unsafe.Pointer(x))
}

func (n *node) atomicLoadNext(l int) *node {
	return (*node)(atomic.LoadPointer(&n.next[l]))
}

func New(seqNo int64) *Memtable {
	h := &node{
		key:   "",
		value: nil,
		next:  make([]unsafe.Pointer, maxLevel),
	}
	return &Memtable{
		head:       h,
		rnd:        rand.New(rand.NewSource(134787)),
		seqNoUpper: seqNo,
	}
}

// Insert inserts (key, timestamp, value) into the memtable.
// Requires that (key, timestamp) does not already exist.
func (m *Memtable) Insert(seqNo int64, key string, timestamp int64, value []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if seqNo < m.seqNoUpper {
		glog.Fatalf("memtable received insert with seqNo less than upper bound: %v < %v", seqNo, m.seqNoUpper)
	}
	m.seqNoUpper = seqNo

	if key == "" {
		glog.Fatal("Invalid empty key.")
	}
	var prev [maxLevel]*node

	n := m.findGreaterOrEqual(key, timestamp, prev[:])

	if n != nil && n.timestamp == timestamp && n.key == key {
		glog.Fatalf("Insert called with duplicate key %v.", key)
	}

	level := m.pickLevel()
	newNode := &node{
		key:       key,
		timestamp: timestamp,
		value:     value,
		next:      make([]unsafe.Pointer, level+1),
	}
	m.size += int64(len(key)) + int64(len(value)) + 8 + 8*int64(level+1)

	for i := 0; i <= level; i++ {
		newNode.atomicStoreNext(i, prev[i].atomicLoadNext(i))
		prev[i].atomicStoreNext(i, newNode)
	}
}

// SizeBytes returns the approximate memory used by this memtable.
func (m *Memtable) SizeBytes() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.size
}

// SequenceUpper returns the largest sequence number applied.
func (m *Memtable) SequenceUpper() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.seqNoUpper
}

// findGreaterOrEqual retuns the first node that is greater than or equal to (key, timestamp).
// according to (key, timestamp) ordering.
// If prev is not nil, filled with the last node visited per level.
func (m *Memtable) findGreaterOrEqual(key string, timestamp int64, prev []*node) *node {
	c := m.head
	var nextAtLevel *node

	for cl := maxLevel - 1; cl >= 0; cl-- {
		nextAtLevel = c.atomicLoadNext(cl)
		for nextAtLevel != nil &&
			(nextAtLevel.key < key || (nextAtLevel.key == key && nextAtLevel.timestamp > timestamp)) {
			c = nextAtLevel
			nextAtLevel = c.atomicLoadNext(cl)
		}

		if prev != nil {
			prev[cl] = c
		}
	}

	return nextAtLevel
}

// Find returns value of key at largest timestamp, which could be nil for a deletion marker.
func (m *Memtable) Find(key string) (value []byte, found bool) {
	if key == "" {
		glog.Fatal("Invalid empty key.")
	}

	n := m.findGreaterOrEqual(key, math.MaxInt64, nil)

	if n != nil && n.key == key {
		return n.value, true
	}
	return nil, false
}

// Iterator iterates entries in the memtable in ascending key order.
// Close() must be called after use.
type Iterator struct {
	m *Memtable
	n *node
}

// NewIterator creates an iterator for this memtable.
func (m *Memtable) NewIterator() *Iterator {
	return &Iterator{
		m: m,
		n: m.head,
	}
}

// Next advances the iterator. Returns true if there is a next value.
func (i *Iterator) Next() bool {
	i.n = i.n.atomicLoadNext(0)
	return i.n != nil
}

// Key returns the current key.
func (i *Iterator) Key() string {
	return i.n.key
}

// Timestamp returns the current timestamp.
func (i *Iterator) Timestamp() int64 {
	return i.n.timestamp
}

// Value returns the current value.
func (i *Iterator) Value() []byte {
	return i.n.value
}

// Close closes the iterator.
func (i *Iterator) Close() {}

// Level assigned to this node, zero indexed.
func (m *Memtable) pickLevel() int {
	var r uint64
	for r == 0 {
		r = uint64(m.rnd.Int63n(int64(1) << maxLevel))
	}
	return bits.TrailingZeros64(r)
}
