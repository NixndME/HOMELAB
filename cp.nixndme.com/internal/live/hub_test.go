package live

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func newTestHub(t *testing.T) (*Hub, *time.Time) {
	t.Helper()
	now := time.Unix(1_700_000_000, 0)
	h := NewHub(Limits{MaxRooms: 3, MaxQueue: 4, MaxMessage: 64, SenderGrace: 30 * time.Second, RoomTTL: time.Hour})
	h.now = func() time.Time { return now }
	return h, &now
}

func offer() Offer {
	return Offer{Device: "Firefox on Linux", Files: 2, Bytes: 10 << 30, Name: "movies"}
}

func TestOpenListJoin(t *testing.T) {
	h, _ := newTestHub(t)
	id, err := h.Open(offer())
	if err != nil {
		t.Fatal(err)
	}
	list := h.Waiting()
	if len(list) != 1 || list[0].ID != id || list[0].Offer.Name != "movies" {
		t.Fatalf("waiting list = %+v", list)
	}
	if err := h.Join(id); err != nil {
		t.Fatal(err)
	}
	if len(h.Waiting()) != 0 {
		t.Fatal("a joined room must leave the waiting list")
	}
	if err := h.Join(id); !errors.Is(err, ErrTaken) {
		t.Fatalf("second receiver: %v, want ErrTaken", err)
	}
	if err := h.Join("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown room: %v, want ErrNotFound", err)
	}
}

func TestJoinTellsTheSender(t *testing.T) {
	h, _ := newTestHub(t)
	id, _ := h.Open(offer())
	sub, err := h.Subscribe(id, Sender)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	if err := h.Join(id); err != nil {
		t.Fatal(err)
	}
	msg := recv(t, sub)
	if string(msg) != `{"type":"joined"}` {
		t.Fatalf("sender got %s", msg)
	}
}

func TestSignalsGoToTheOtherSide(t *testing.T) {
	h, _ := newTestHub(t)
	id, _ := h.Open(offer())
	_ = h.Join(id)
	snd, _ := h.Subscribe(id, Sender)
	defer snd.Close()
	rcv, _ := h.Subscribe(id, Receiver)
	defer rcv.Close()
	<-snd.C // the "joined" notice

	if err := h.Signal(id, Sender, json.RawMessage(`{"sdp":"offer"}`)); err != nil {
		t.Fatal(err)
	}
	if got := recv(t, rcv); string(got) != `{"sdp":"offer"}` {
		t.Fatalf("receiver got %s", got)
	}
	if err := h.Signal(id, Receiver, json.RawMessage(`{"sdp":"answer"}`)); err != nil {
		t.Fatal(err)
	}
	if got := recv(t, snd); string(got) != `{"sdp":"answer"}` {
		t.Fatalf("sender got %s", got)
	}
}

func TestMessagesWaitForASubscriber(t *testing.T) {
	h, _ := newTestHub(t)
	id, _ := h.Open(offer())
	_ = h.Join(id)
	_ = h.Signal(id, Sender, json.RawMessage(`1`))
	_ = h.Signal(id, Sender, json.RawMessage(`2`))
	rcv, _ := h.Subscribe(id, Receiver)
	defer rcv.Close()
	if a, b := recv(t, rcv), recv(t, rcv); string(a) != "1" || string(b) != "2" {
		t.Fatalf("queued messages out of order: %s %s", a, b)
	}
}

func TestSignalLimits(t *testing.T) {
	h, _ := newTestHub(t)
	id, _ := h.Open(offer())
	if err := h.Signal(id, Sender, json.RawMessage(`1`)); !errors.Is(err, ErrNoPeer) {
		t.Fatalf("signal before a receiver joined: %v, want ErrNoPeer", err)
	}
	_ = h.Join(id)
	big := make([]byte, 65)
	for i := range big {
		big[i] = '1'
	}
	if err := h.Signal(id, Sender, big); !errors.Is(err, ErrTooBig) {
		t.Fatalf("oversized message: %v, want ErrTooBig", err)
	}
	for i := 0; i < 4; i++ {
		if err := h.Signal(id, Sender, json.RawMessage(`1`)); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.Signal(id, Sender, json.RawMessage(`1`)); !errors.Is(err, ErrBusy) {
		t.Fatalf("full queue: %v, want ErrBusy", err)
	}
}

func TestRoomLimit(t *testing.T) {
	h, _ := newTestHub(t)
	for i := 0; i < 3; i++ {
		if _, err := h.Open(offer()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.Open(offer()); !errors.Is(err, ErrFull) {
		t.Fatalf("4th room: %v, want ErrFull", err)
	}
}

func TestCloseEndsSubscriptions(t *testing.T) {
	h, _ := newTestHub(t)
	id, _ := h.Open(offer())
	_ = h.Join(id)
	rcv, _ := h.Subscribe(id, Receiver)
	h.Close(id)
	select {
	case _, ok := <-rcv.C:
		if ok {
			t.Fatal("expected the channel to close")
		}
	case <-time.After(time.Second):
		t.Fatal("subscription not closed")
	}
	if _, err := h.Subscribe(id, Receiver); !errors.Is(err, ErrNotFound) {
		t.Fatalf("subscribe to closed room: %v", err)
	}
}

func TestOnlyOneSubscriberPerSide(t *testing.T) {
	h, _ := newTestHub(t)
	id, _ := h.Open(offer())
	first, _ := h.Subscribe(id, Sender)
	second, err := h.Subscribe(id, Sender)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	select {
	case _, ok := <-first.C:
		if ok {
			t.Fatal("old subscription should be closed, not fed")
		}
	case <-time.After(time.Second):
		t.Fatal("a reconnecting tab must replace the old subscription")
	}
	first.Close() // closing a replaced subscription must not detach the new one
	_ = h.Join(id)
	if got := recv(t, second); string(got) != `{"type":"joined"}` {
		t.Fatalf("new subscription got %s", got)
	}
}

func TestSweepDropsAbandonedRooms(t *testing.T) {
	h, now := newTestHub(t)
	idle, _ := h.Open(offer()) // the sender never connects
	kept, _ := h.Open(offer())
	sub, _ := h.Subscribe(kept, Sender)
	defer sub.Close()

	*now = now.Add(31 * time.Second)
	h.Sweep()
	if _, err := h.Subscribe(idle, Sender); !errors.Is(err, ErrNotFound) {
		t.Fatal("a room whose sender is gone must be swept after the grace period")
	}
	if len(h.Waiting()) != 1 {
		t.Fatal("a room with a connected sender must stay")
	}

	*now = now.Add(time.Hour)
	h.Sweep()
	if len(h.Waiting()) != 0 {
		t.Fatal("rooms must not outlive RoomTTL")
	}
}

func TestSenderDisconnectStartsGrace(t *testing.T) {
	h, now := newTestHub(t)
	id, _ := h.Open(offer())
	sub, _ := h.Subscribe(id, Sender)
	*now = now.Add(time.Minute)
	sub.Close()
	*now = now.Add(10 * time.Second)
	h.Sweep()
	if len(h.Waiting()) != 1 {
		t.Fatal("a brief reconnect must not kill the room")
	}
	*now = now.Add(25 * time.Second)
	h.Sweep()
	if len(h.Waiting()) != 0 {
		t.Fatal("room must go once the sender has been away longer than the grace period")
	}
}

func recv(t *testing.T, s *Subscription) []byte {
	t.Helper()
	select {
	case m, ok := <-s.C:
		if !ok {
			t.Fatal("subscription closed")
		}
		return m
	case <-time.After(time.Second):
		t.Fatal("no message")
	}
	return nil
}

func TestCloseAll(t *testing.T) {
	h, _ := newTestHub(t)
	a, _ := h.Open(offer())
	sub, _ := h.Subscribe(a, Sender)
	_, _ = h.Open(offer())
	h.CloseAll()
	if _, ok := <-sub.C; ok {
		t.Fatal("subscription must close")
	}
	if len(h.Waiting()) != 0 {
		t.Fatal("rooms left after CloseAll")
	}
}
