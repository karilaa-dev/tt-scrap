package core

// Expiry order is independent of access order, so inserts purge expired entries
// without scanning the live LRU list. There is one heap node per cached entry.
type expiryHeap[T any] []*cacheItem[T]

func (h expiryHeap[T]) Len() int           { return len(h) }
func (h expiryHeap[T]) Less(i, j int) bool { return h[i].expires.Before(h[j].expires) }
func (h expiryHeap[T]) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].expiryIndex, h[j].expiryIndex = i, j
}
func (h *expiryHeap[T]) Push(value any) {
	entry := value.(*cacheItem[T])
	entry.expiryIndex = len(*h)
	*h = append(*h, entry)
}
func (h *expiryHeap[T]) Pop() any {
	last := len(*h) - 1
	entry := (*h)[last]
	(*h)[last] = nil
	*h = (*h)[:last]
	entry.expiryIndex = -1
	return entry
}
