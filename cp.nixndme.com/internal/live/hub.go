// Package live is the meeting point for Live Transfer. Two browsers find each other here and
// swap the small WebRTC handshake messages (SDP offer/answer, ICE candidates). The file bytes
// never pass through the server: they go browser to browser over the local network.
//
// Everything is in memory. A restart only interrupts transfers that are still connecting.
package live

import (
	"errors"
	"sort"
	"sync"
	"time"

	"cpshare/internal/ids"
)

// Role is one end of a transfer.
type Role int

const (
	Sender Role = iota
	Receiver
)

func (r Role) other() Role { return 1 - r }

var (
	ErrNotFound = errors.New("live transfer not found")
	ErrTaken    = errors.New("another device is already receiving this transfer")
	ErrFull     = errors.New("too many live transfers open")
	ErrNoPeer   = errors.New("no receiver has joined yet")
	ErrTooBig   = errors.New("signal message too large")
	ErrBusy     = errors.New("the other device is not reading its messages")
)

// joinedNotice tells the sender that a receiver arrived and it can start the handshake.
var joinedNotice = []byte(`{"type":"joined"}`)

// Limits bounds what one hub may hold, so a stuck tab cannot grow memory.
type Limits struct {
	MaxRooms    int
	MaxQueue    int           // pending messages per side
	MaxMessage  int           // bytes per message
	SenderGrace time.Duration // how long a room survives without a connected sender
	RoomTTL     time.Duration
}

// Offer is what the receiving device sees before it accepts.
type Offer struct {
	Device string `json:"device"`
	Name   string `json:"name"`
	Files  int    `json:"files"`
	Bytes  int64  `json:"bytes"`
	Folder bool   `json:"folder"` // a picked folder: the receiver keeps its layout
}

// RoomInfo is one transfer that is waiting for a receiver.
type RoomInfo struct {
	ID      string `json:"id"`
	Offer   Offer  `json:"offer"`
	Created int64  `json:"created"`
}

type side struct {
	queue [][]byte      // messages that arrived while nobody was listening
	sub   *Subscription // the tab listening now, if any
	gone  time.Time     // when the last subscription ended
}

type room struct {
	info   RoomInfo
	joined bool
	sides  [2]side
}

// Hub holds every open transfer.
type Hub struct {
	lim   Limits
	mu    sync.Mutex
	rooms map[string]*room
	now   func() time.Time
}

// NewHub returns an empty hub.
func NewHub(lim Limits) *Hub {
	return &Hub{lim: lim, rooms: map[string]*room{}, now: time.Now}
}

// Subscription delivers one side's messages. C is closed when the room ends or another tab of
// the same side takes over.
type Subscription struct {
	C      <-chan []byte
	c      chan []byte
	hub    *Hub
	id     string
	role   Role
	closed bool // guarded by hub.mu
}

// Open starts a transfer and returns its id.
func (h *Hub) Open(o Offer) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.rooms) >= h.lim.MaxRooms {
		return "", ErrFull
	}
	now := h.now()
	id := ids.Share()
	r := &room{info: RoomInfo{ID: id, Offer: o, Created: now.Unix()}}
	r.sides[Sender].gone = now
	r.sides[Receiver].gone = now
	h.rooms[id] = r
	return id, nil
}

// Waiting lists transfers no device has accepted yet, newest first.
func (h *Hub) Waiting() []RoomInfo {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := []RoomInfo{}
	for _, r := range h.rooms {
		if !r.joined {
			out = append(out, r.info)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created > out[j].Created })
	return out
}

// Join claims a transfer for the receiving device. Only one device may receive.
func (h *Hub) Join(id string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	r, ok := h.rooms[id]
	if !ok {
		return ErrNotFound
	}
	if r.joined {
		return ErrTaken
	}
	r.joined = true
	_ = h.deliver(r, Sender, joinedNotice) // a full queue only means the sender is gone
	return nil
}

// Signal passes one handshake message from role to the other side.
func (h *Hub) Signal(id string, from Role, msg []byte) error {
	if len(msg) > h.lim.MaxMessage {
		return ErrTooBig
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	r, ok := h.rooms[id]
	if !ok {
		return ErrNotFound
	}
	if !r.joined {
		return ErrNoPeer
	}
	return h.deliver(r, from.other(), append([]byte(nil), msg...))
}

// deliver hands msg to the listening tab, or queues it until one connects. Caller holds h.mu.
func (h *Hub) deliver(r *room, to Role, msg []byte) error {
	s := &r.sides[to]
	if s.sub != nil {
		select {
		case s.sub.c <- msg:
			return nil
		default:
			return ErrBusy
		}
	}
	if len(s.queue) >= h.lim.MaxQueue {
		return ErrBusy
	}
	s.queue = append(s.queue, msg)
	return nil
}

// Subscribe attaches a tab to one side of a transfer. A second tab of the same side replaces
// the first (EventSource reconnects look exactly like that).
func (h *Hub) Subscribe(id string, role Role) (*Subscription, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	r, ok := h.rooms[id]
	if !ok {
		return nil, ErrNotFound
	}
	s := &r.sides[role]
	if s.sub != nil {
		h.detach(s.sub)
	}
	c := make(chan []byte, h.lim.MaxQueue)
	for _, m := range s.queue {
		c <- m
	}
	s.queue = nil
	sub := &Subscription{C: c, c: c, hub: h, id: id, role: role}
	s.sub = sub
	return sub, nil
}

// Close stops listening. It is safe to call more than once and after the room ended.
func (s *Subscription) Close() {
	s.hub.mu.Lock()
	defer s.hub.mu.Unlock()
	s.hub.detach(s)
}

// detach ends a subscription and records when its side went away. Caller holds h.mu.
func (h *Hub) detach(sub *Subscription) {
	if sub.closed {
		return
	}
	sub.closed = true
	close(sub.c)
	if r, ok := h.rooms[sub.id]; ok && r.sides[sub.role].sub == sub {
		r.sides[sub.role].sub = nil
		r.sides[sub.role].gone = h.now()
	}
}

// Close ends a transfer for both sides.
func (h *Hub) Close(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.remove(id)
}

// CloseAll ends every transfer (used at shutdown).
func (h *Hub) CloseAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for id := range h.rooms {
		h.remove(id)
	}
}

func (h *Hub) remove(id string) {
	r, ok := h.rooms[id]
	if !ok {
		return
	}
	for i := range r.sides {
		if r.sides[i].sub != nil {
			h.detach(r.sides[i].sub)
		}
	}
	delete(h.rooms, id)
}

// Sweep removes transfers whose sender left more than SenderGrace ago, and any older than RoomTTL.
func (h *Hub) Sweep() {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	for id, r := range h.rooms {
		snd := r.sides[Sender]
		abandoned := snd.sub == nil && now.Sub(snd.gone) > h.lim.SenderGrace
		expired := now.Sub(time.Unix(r.info.Created, 0)) > h.lim.RoomTTL
		if abandoned || expired {
			h.remove(id)
		}
	}
}
