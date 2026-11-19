package dispatch

import (
	"cmp"
	"slices"
	"time"

	"telegram-messages-sender/internal/app"
	"telegram-messages-sender/internal/domain"
	"telegram-messages-sender/internal/store"
)

type occurrence struct {
	pair          store.Pair
	publication   domain.Publication
	firstDue      time.Time
	due           time.Time
	retryAt       time.Time
	failures      int
	blockedReason string
	history       store.PairDelivery
}

type schedule map[store.Pair]*occurrence

func (s schedule) refresh(snapshot store.Snapshot, delivery store.DeliveryState, now time.Time, draw func(int) int) {
	disabled := make(map[domain.ChatID]bool, len(snapshot.DisabledChats))
	for _, chat := range snapshot.DisabledChats {
		disabled[chat] = true
	}
	active := make(map[store.Pair]bool)
	for _, publication := range snapshot.Publications {
		if publication.State != domain.Enabled {
			continue
		}
		first := true
		for _, chat := range publication.Destinations {
			if disabled[chat] {
				continue
			}
			pair := store.Pair{PublicationID: publication.ID, ChatID: chat}
			active[pair] = true
			history := delivery.Pairs[pair]
			item, exists := s[pair]
			if !exists {
				firstDue := now
				if !first {
					firstDue = now.Add(time.Duration(30+draw(61)) * time.Second)
				}
				item = &occurrence{pair: pair, firstDue: firstDue}
				s[pair] = item
			}
			first = false
			item.refresh(publication, history, now, exists)
		}
	}
	for pair := range s {
		if !active[pair] {
			delete(s, pair)
		}
	}
}

func (o *occurrence) refresh(publication domain.Publication, history store.PairDelivery, now time.Time, exists bool) {
	intervalChanged := exists && o.publication.Interval != publication.Interval
	outcomeChanged := !exists || o.history.LastOutcome != history.LastOutcome || !o.history.LastCompleted.Equal(history.LastCompleted)
	confirmationChanged := !o.history.ConfirmedAt.Equal(history.ConfirmedAt)
	o.publication = publication
	o.history = history

	switch {
	case history.LastOutcome == domain.AttemptPending:
		o.due = time.Time{}
	case history.LastOutcome == domain.AttemptUnknown:
		if intervalChanged || outcomeChanged || confirmationChanged {
			anchor := history.ConfirmedAt
			if anchor.IsZero() {
				anchor = history.FirstDue
			}
			reference := now
			if exists && outcomeChanged && !intervalChanged && !history.LastCompleted.IsZero() {
				reference = history.LastCompleted
			}
			o.due = nextSlot(anchor, publication.Interval, reference)
			o.blockedReason = ""
			o.failures = 0
			o.retryAt = time.Time{}
		}
	case !history.ConfirmedAt.IsZero():
		o.due = later(o.firstDue, history.ConfirmedAt.Add(publication.Interval))
		if outcomeChanged && history.LastOutcome == domain.AttemptSuccess {
			o.blockedReason = ""
			o.failures = 0
			o.retryAt = time.Time{}
		}
	default:
		o.due = o.firstDue
	}
}

func nextSlot(anchor time.Time, interval time.Duration, now time.Time) time.Time {
	if now.Before(anchor) {
		return anchor
	}
	elapsed := now.Sub(anchor)
	return now.Add(interval - elapsed%interval)
}

func (o *occurrence) delayRetry(now time.Time) {
	delay := 5 * time.Second << min(o.failures, 4)
	o.failures = min(o.failures+1, 4)
	o.retryAt = now.Add(min(delay, time.Minute))
}

func (s schedule) next(now time.Time, waits map[domain.ChatID]time.Time) (*occurrence, time.Time) {
	var selected *occurrence
	var wake time.Time
	for _, item := range s {
		if item.due.IsZero() {
			continue
		}
		eligible := later(later(item.due, item.retryAt), later(waits[""], waits[item.pair.ChatID]))
		if eligible.After(now) {
			if wake.IsZero() || eligible.Before(wake) {
				wake = eligible
			}
			continue
		}
		if selected == nil || item.due.Before(selected.due) ||
			(item.due.Equal(selected.due) && comparePairs(item.pair, selected.pair) < 0) {
			selected = item
		}
	}
	return selected, wake
}

func (s schedule) status(now time.Time, history map[store.Pair]store.PairDelivery) []app.DispatchStatus {
	pairs := make([]store.Pair, 0, len(s)+len(history))
	for pair := range s {
		pairs = append(pairs, pair)
	}
	for pair := range history {
		if s[pair] == nil {
			pairs = append(pairs, pair)
		}
	}
	slices.SortFunc(pairs, comparePairs)
	result := make([]app.DispatchStatus, 0, len(pairs))
	for _, pair := range pairs {
		state := history[pair]
		status := app.DispatchStatus{PublicationID: pair.PublicationID, ChatID: pair.ChatID, Outcome: state.LastOutcome, Reason: state.Reason}
		if item := s[pair]; item != nil && state.LastOutcome != domain.AttemptPending && !item.due.IsZero() {
			if item.blockedReason != "" {
				status.Reason = item.blockedReason
			}
			due := item.due
			status.DueAt = &due
			status.Delay = max(0, now.Sub(item.due))
		}
		result = append(result, status)
	}
	return result
}

func comparePairs(a, b store.Pair) int {
	if c := cmp.Compare(a.PublicationID, b.PublicationID); c != 0 {
		return c
	}
	return cmp.Compare(a.ChatID, b.ChatID)
}

func later(a, b time.Time) time.Time {
	if a.Before(b) {
		return b
	}
	return a
}
