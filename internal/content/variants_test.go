package content

import (
	"context"
	"errors"
	"testing"

	"telegram-messages-sender/internal/domain"
)

type sourceFixture func(context.Context, domain.SavedMessage) (Kind, error)

func (f sourceFixture) ResolveSaved(ctx context.Context, ref domain.SavedMessage) (Kind, error) {
	return f(ctx, ref)
}

func TestPrepareUsesCurrentSourceAndLastConfirmedForEachDestination(t *testing.T) {
	variantSource := domain.SavedMessage{AccountID: 42, MessageID: 81}
	current := Photo
	var lookups int
	selector := NewSelector(sourceFixture(func(_ context.Context, ref domain.SavedMessage) (Kind, error) {
		lookups++
		if ref != variantSource {
			t.Errorf("unexpected account or message ID: %v", ref)
		}
		return current, nil
	}))
	draws := []int{0, 1}
	selector.draw = func(n int) int {
		if n != 2 || len(draws) == 0 {
			t.Fatalf("expected a fresh draw from two candidates")
		}
		draw := draws[0]
		draws = draws[1:]
		return draw
	}
	variants := []domain.Variant{
		{ID: "a", Text: "manual"},
		{ID: "b", SavedMessage: &variantSource},
		{ID: "c", Text: "other"},
	}

	for _, want := range []struct {
		last    domain.VariantID
		id      domain.VariantID
		kind    Kind
		current Kind
	}{
		{last: "a", id: "b", kind: Photo, current: Photo},
		{last: "a", id: "c", kind: Text, current: Video},
		{last: "c", id: "b", kind: Video, current: Video},
	} {
		current = want.current
		candidates := variants
		if want.last == "c" {
			candidates = variants[1:]
		}
		got, err := selector.Prepare(t.Context(), candidates, want.last)
		if err != nil || got.Variant.ID != want.id || got.Variant.Kind != want.kind {
			t.Fatalf("last confirmed %q prepared %v, %v", want.last, got.Variant, err)
		}
		if got.Variant.ID == "b" && (got.Variant.SavedMessage == nil || *got.Variant.SavedMessage != variantSource) {
			t.Fatal("preparation lost the saved reference")
		}
	}
	if lookups != 2 || len(draws) != 0 {
		t.Fatalf("used %d lookups and left %d draws unused", lookups, len(draws))
	}
}

func TestPrepareFallbackAndExhaustion(t *testing.T) {
	manual := domain.Variant{ID: "a", Text: "fallback"}
	ref := domain.SavedMessage{AccountID: 42, MessageID: 81}
	saved := domain.Variant{ID: "b", SavedMessage: &ref}
	for _, reason := range []error{ErrSourceDeleted, ErrSourceProtected, ErrSourceUnsupported} {
		t.Run(reason.Error(), func(t *testing.T) {
			selector := NewSelector(sourceFixture(func(context.Context, domain.SavedMessage) (Kind, error) {
				return "", reason
			}))
			got, err := selector.Prepare(t.Context(), []domain.Variant{manual, saved}, manual.ID)
			if err != nil || got.Variant.ID != manual.ID || got.Variant.Text != manual.Text {
				t.Fatalf("no fallback to last confirmed: %v, %v", got, err)
			}
			if len(got.Unavailable) != 1 || got.Unavailable[0].ID != saved.ID || got.Unavailable[0].Reason != reason {
				t.Fatalf("lost unavailable reason: %v", got.Unavailable)
			}
			got, err = selector.Prepare(t.Context(), []domain.Variant{saved}, "")
			if !errors.Is(err, ErrVariantsExhausted) || len(got.Unavailable) != 1 {
				t.Fatalf("empty draw did not report exhaustion: %v, %v", got, err)
			}
		})
	}
}

func TestPrepareKeepsVariantAfterTemporaryFailureAndRepeatsOnlyChoice(t *testing.T) {
	ref := domain.SavedMessage{AccountID: 42, MessageID: 81}
	offline := true
	selector := NewSelector(sourceFixture(func(context.Context, domain.SavedMessage) (Kind, error) {
		if offline {
			return "", ErrSourceUnavailable
		}
		return Text, nil
	}))
	variants := []domain.Variant{{ID: "only", SavedMessage: &ref}}

	got, err := selector.Prepare(t.Context(), variants, "only")
	if !errors.Is(err, ErrSourceUnavailable) || len(got.Unavailable) != 0 {
		t.Fatalf("temporary failure invalidated the variant: %v, %v", got, err)
	}
	offline = false
	got, err = selector.Prepare(t.Context(), variants, "only")
	if err != nil || got.Variant.ID != "only" {
		t.Fatalf("only eligible variant could not repeat: %v, %v", got, err)
	}
}
