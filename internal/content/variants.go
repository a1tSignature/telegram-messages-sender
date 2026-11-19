package content

import (
	"context"
	"errors"
	"math/rand/v2"

	"telegram-messages-sender/internal/domain"
)

type Kind string

const (
	Text  Kind = "text"
	Photo Kind = "photo"
	Video Kind = "video"
)

var (
	ErrSourceDeleted     = errors.New("Saved Messages source was deleted")
	ErrSourceUnavailable = errors.New("Saved Messages lookup unavailable")
	ErrSourceProtected   = errors.New("Saved Messages source is protected")
	ErrSourceUnsupported = errors.New("Saved Messages source type is unsupported")
	ErrSourceAccount     = errors.New("Saved Messages source belongs to a different account")
	ErrVariantsExhausted = errors.New("no available content variants")
)

type Source interface {
	ResolveSaved(ctx context.Context, ref domain.SavedMessage) (Kind, error)
}

type PreparedVariant struct {
	ID           domain.VariantID
	Kind         Kind
	Text         string
	SavedMessage *domain.SavedMessage
}

type UnavailableVariant struct {
	ID     domain.VariantID
	Reason error
}

type Preparation struct {
	Variant     PreparedVariant
	Unavailable []UnavailableVariant
}

type Selector struct {
	source Source
	draw   func(int) int
}

func NewSelector(source Source) Selector {
	return Selector{source: source, draw: rand.IntN}
}

// Prepare uses the last confirmed variant for the current publication and destination.
// The caller owns that history and updates it only after confirmed delivery.
func (s Selector) Prepare(ctx context.Context, variants []domain.Variant, lastConfirmed domain.VariantID) (Preparation, error) {
	var result Preparation
	var previous *domain.Variant
	candidates := make([]domain.Variant, 0, len(variants))
	for i, variant := range variants {
		if variant.ID == lastConfirmed {
			previous = &variants[i]
			continue
		}
		candidates = append(candidates, variant)
	}

	for len(candidates) > 0 || previous != nil {
		if err := ctx.Err(); err != nil {
			return result, err
		}

		var candidate domain.Variant
		switch len(candidates) {
		case 0:
			candidate = *previous
			previous = nil
		case 1:
			candidate = candidates[0]
			candidates = candidates[:0]
		default:
			index := s.draw(len(candidates))
			candidate = candidates[index]
			last := len(candidates) - 1
			candidates[index] = candidates[last]
			candidates = candidates[:last]
		}

		prepared, err := s.prepare(ctx, candidate)
		switch {
		case err == nil:
			result.Variant = prepared
			return result, nil
		case errors.Is(err, ErrSourceDeleted), errors.Is(err, ErrSourceProtected), errors.Is(err, ErrSourceUnsupported):
			result.Unavailable = append(result.Unavailable, UnavailableVariant{ID: candidate.ID, Reason: err})
		default:
			return result, err
		}
	}

	return result, ErrVariantsExhausted
}

func (s Selector) prepare(ctx context.Context, variant domain.Variant) (PreparedVariant, error) {
	if variant.SavedMessage == nil {
		if variant.Text == "" {
			return PreparedVariant{}, ErrSourceUnsupported
		}
		return PreparedVariant{ID: variant.ID, Kind: Text, Text: variant.Text}, nil
	}

	ref := *variant.SavedMessage
	kind, err := s.source.ResolveSaved(ctx, ref)
	if err != nil {
		return PreparedVariant{}, err
	}
	return PreparedVariant{ID: variant.ID, Kind: kind, SavedMessage: &ref}, nil
}
