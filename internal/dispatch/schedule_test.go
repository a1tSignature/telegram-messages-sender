package dispatch

import (
	"testing"
	"time"

	"telegram-messages-sender/internal/domain"
	"telegram-messages-sender/internal/store"
)

func TestCommonStartOldestDueAndCoalescing(t *testing.T) {
	start := time.Unix(1_800_000_000, 0)
	queue := make(schedule)
	state := store.Snapshot{Publications: []domain.Publication{
		{ID: "z", State: domain.Enabled, Interval: time.Minute, Destinations: []domain.ChatID{"group:9", "group:2", "group:3", "group:4"}},
		{ID: "a", State: domain.Enabled, Interval: time.Minute, Destinations: []domain.ChatID{"group:7"}},
	}, DisabledChats: []domain.ChatID{"group:9"}}
	draws := []int{60, 0}
	draw := func(n int) int {
		if n != 61 || len(draws) == 0 {
			t.Fatal("unexpected extra random offset")
		}
		value := draws[0]
		draws = draws[1:]
		return value
	}
	queue.refresh(state, store.DeliveryState{}, start, draw)
	for _, check := range []struct {
		pair   store.Pair
		offset time.Duration
	}{
		{store.Pair{PublicationID: "z", ChatID: "group:2"}, 0},
		{store.Pair{PublicationID: "z", ChatID: "group:3"}, 90 * time.Second},
		{store.Pair{PublicationID: "z", ChatID: "group:4"}, 30 * time.Second},
	} {
		if got := queue[check.pair].due; !got.Equal(start.Add(check.offset)) {
			t.Fatalf("%+v due at %v", check.pair, got)
		}
	}
	late := start.Add(5 * time.Minute)
	queue.refresh(state, store.DeliveryState{}, late, draw)
	if len(queue) != 4 || len(draws) != 0 {
		t.Fatalf("missed slots produced extra work: %d jobs", len(queue))
	}
	item, _ := queue.next(late, map[domain.ChatID]time.Time{"group:7": late.Add(time.Second)})
	if item.pair.PublicationID != "z" || item.pair.ChatID != "group:2" {
		t.Fatalf("wait did not skip the affected chat: %+v", item)
	}
	item, wake := queue.next(late, map[domain.ChatID]time.Time{"": late.Add(time.Second)})
	if item != nil || !wake.Equal(late.Add(time.Second)) {
		t.Fatalf("account-wide wait did not hold every pair: %+v, %v", item, wake)
	}
	item, _ = queue.next(late, nil)
	if item.pair.PublicationID != "a" {
		t.Fatalf("stable tie breaker selected %+v", item.pair)
	}
	if got := queue.status(late, nil); got[0].Delay != 5*time.Minute {
		t.Fatalf("queue delay: %+v", got[0])
	}
}

func TestIntervalChangesRecomputeConfirmedAndUnknownCadence(t *testing.T) {
	start := time.Unix(1_800_000_000, 0)
	pair := store.Pair{PublicationID: "a", ChatID: "group:1"}
	publication := domain.Publication{ID: pair.PublicationID, State: domain.Enabled, Interval: 20 * time.Minute, Destinations: []domain.ChatID{pair.ChatID}}
	snapshot := store.Snapshot{Publications: []domain.Publication{publication}}
	queue := make(schedule)
	draw := func(int) int { t.Fatal("unexpected offset for only destination"); return 0 }
	confirmed := store.PairDelivery{FirstDue: start.Add(-time.Minute), ConfirmedAt: start, LastOutcome: domain.AttemptSuccess, LastCompleted: start}
	delivery := store.DeliveryState{Pairs: map[store.Pair]store.PairDelivery{pair: confirmed}}

	now := start.Add(10 * time.Minute)
	queue.refresh(snapshot, delivery, now, draw)
	if queue[pair].due != start.Add(20*time.Minute) {
		t.Fatal("repeat was not anchored to success")
	}
	snapshot.Publications[0].Interval = 5 * time.Minute
	queue.refresh(snapshot, delivery, now, draw)
	if item, _ := queue.next(now, nil); item == nil || len(queue) != 1 {
		t.Fatal("shortened interval should allow one overdue send")
	}

	snapshot.Publications[0].Interval = 20 * time.Minute
	now = start.Add(36 * time.Minute)
	confirmed.LastOutcome, confirmed.LastCompleted = domain.AttemptUnknown, now
	delivery.Pairs[pair] = confirmed
	queue.refresh(snapshot, delivery, now, draw)
	if queue[pair].due != start.Add(40*time.Minute) {
		t.Fatal("unknown delivery shifted the confirmed anchor")
	}
	snapshot.Publications[0].Interval = 15 * time.Minute
	queue.refresh(snapshot, delivery, now, draw)
	due := start.Add(45 * time.Minute)
	queue.refresh(snapshot, delivery, due, draw)
	if item, _ := queue.next(due, nil); item == nil || item.due != due {
		t.Fatal("unknown cadence slot moved again when it became due")
	}

	firstOnly := store.PairDelivery{FirstDue: start.Add(time.Minute), LastOutcome: domain.AttemptUnknown, LastCompleted: now}
	delivery.Pairs[pair] = firstOnly
	queue.refresh(snapshot, delivery, now, draw)
	if queue[pair].due != start.Add(46*time.Minute) {
		t.Fatal("unknown delivery without success lost its first scheduled anchor")
	}
}
