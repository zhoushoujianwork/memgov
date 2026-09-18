package runtime

import "sync"

// WakeBus broadcasts a committed channel intake to every runtime consuming
// that channel. Notifications are deliberately coalesced: SQLite remains the
// source of truth, so one wake is enough for a worker to drain all ready work.
type WakeBus struct {
	mu   sync.Mutex
	next uint64
	subs map[string]map[uint64]chan struct{}
}

func NewWakeBus() *WakeBus {
	return &WakeBus{subs: map[string]map[uint64]chan struct{}{}}
}

func (b *WakeBus) Subscribe(channelID string) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	if b == nil || channelID == "" {
		return ch, func() {}
	}
	b.mu.Lock()
	b.next++
	id := b.next
	if b.subs[channelID] == nil {
		b.subs[channelID] = map[uint64]chan struct{}{}
	}
	b.subs[channelID][id] = ch
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		delete(b.subs[channelID], id)
		if len(b.subs[channelID]) == 0 {
			delete(b.subs, channelID)
		}
		b.mu.Unlock()
	}
}

func (b *WakeBus) Notify(channelID string) {
	if b == nil || channelID == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, ch := range b.subs[channelID] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}
