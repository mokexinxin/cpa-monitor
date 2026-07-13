package notification

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
)

// NamedAlertSender gives a transport a stable, non-secret log name.
type NamedAlertSender struct {
	Name   string
	Sender AlertSender
}

// Router sends to the primary transport and uses fallback only after primary
// failure. A successful fallback is a successful delivery; the primary is not
// replayed later for that alert occurrence.
type Router struct {
	primary  NamedAlertSender
	fallback *NamedAlertSender
	logger   *slog.Logger
}

func NewRouter(primary NamedAlertSender, fallback *NamedAlertSender, logger *slog.Logger) (*Router, error) {
	primary.Name = strings.TrimSpace(primary.Name)
	if primary.Name == "" || primary.Sender == nil {
		return nil, errors.New("notification primary sender is required")
	}
	if fallback != nil {
		copy := *fallback
		copy.Name = strings.TrimSpace(copy.Name)
		if copy.Name == "" || copy.Sender == nil {
			return nil, errors.New("notification fallback sender is invalid")
		}
		if copy.Name == primary.Name {
			return nil, errors.New("notification fallback must differ from primary")
		}
		fallback = &copy
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Router{primary: primary, fallback: fallback, logger: logger}, nil
}

func (r *Router) SendBatch(ctx context.Context, batch Batch) error {
	if ctx == nil {
		return errors.New("notification context must not be nil")
	}
	if r == nil || r.primary.Sender == nil {
		return errors.New("notification router is not initialized")
	}
	primaryErr := r.primary.Sender.SendBatch(ctx, batch)
	if primaryErr == nil {
		return nil
	}
	if r.fallback == nil || ctx.Err() != nil {
		return fmt.Errorf("send notification through %s: %w", r.primary.Name, primaryErr)
	}
	fallbackErr := r.fallback.Sender.SendBatch(ctx, batch)
	if fallbackErr != nil {
		return errors.Join(
			fmt.Errorf("send notification through %s: %w", r.primary.Name, primaryErr),
			fmt.Errorf("send notification through %s: %w", r.fallback.Name, fallbackErr),
		)
	}
	r.logger.WarnContext(ctx, "primary notification channel failed; fallback delivered",
		"primary_channel", r.primary.Name,
		"fallback_channel", r.fallback.Name,
		"scope", batch.Scope,
		"kind", batch.Kind,
	)
	return nil
}

var _ AlertSender = (*Router)(nil)
