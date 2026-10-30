package web

import "sync"

// hub weckt wartende Handler (Long-Poll der Agenten, Live-Ansicht eines Auftrags), wenn sich etwas geändert hat.
// Nur im Speicher und nur für eine Cockpit-Instanz; wer verpasst, fragt beim nächsten Zeitablauf ohnehin den Speicher.
type hub struct {
	mu   sync.Mutex
	subs map[string]map[chan struct{}]struct{}
}

// subscribe liefert einen Kanal, der bei notify(key) ein Signal bekommt (höchstens eins wartet), und die Abmeldung.
func (h *hub) subscribe(key string) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	h.mu.Lock()
	if h.subs == nil {
		h.subs = map[string]map[chan struct{}]struct{}{}
	}
	if h.subs[key] == nil {
		h.subs[key] = map[chan struct{}]struct{}{}
	}
	h.subs[key][ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		delete(h.subs[key], ch)
		if len(h.subs[key]) == 0 {
			delete(h.subs, key)
		}
		h.mu.Unlock()
	}
}

func (h *hub) notify(key string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs[key] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}
