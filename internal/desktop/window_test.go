package desktop

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	fynetest "fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/widget"

	"telegram-messages-sender/internal/app"
	"telegram-messages-sender/internal/content"
	"telegram-messages-sender/internal/control"
	"telegram-messages-sender/internal/dispatch"
	"telegram-messages-sender/internal/domain"
	"telegram-messages-sender/internal/store"
	"telegram-messages-sender/internal/telegram"
)

func TestWindowAndConsoleShareOwnerAndStopOnClose(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	defer cancel()
	application := fynetest.NewApp()
	dir := filepath.Join(t.TempDir(), "private")
	owner, err := control.Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	state, err := store.Open(ctx, filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	engine := dispatch.New(state)
	if _, err := engine.SavePublication(ctx, 0, domain.Publication{ID: "news", State: domain.Enabled, Interval: time.Minute}); err != nil {
		t.Fatal(err)
	}
	if _, err := state.BindAccount(ctx, 42); err != nil {
		t.Fatal(err)
	}
	ownerCtx, shutdown := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- owner.Serve(ownerCtx, engine.Handler()) }()
	defer shutdown()
	view := NewWindow(ownerCtx, application, Backend{Command: engine.Handler()}, shutdown)
	waitStatus := func(state domain.PublicationState) app.Result {
		t.Helper()
		tick := time.NewTicker(5 * time.Millisecond)
		defer tick.Stop()
		for {
			result, err := control.Call(ctx, dir, app.Command{Operation: app.Status})
			if err == nil && len(result.Publications) == 1 && result.Publications[0].State == state {
				return result
			}
			select {
			case <-ctx.Done():
				t.Fatalf("loopback owner status = %+v, %v, expected %s", result, err, state)
			case <-tick.C:
			}
		}
	}
	initial := waitStatus(domain.Enabled)
	fyne.DoAndWait(func() {
		view.render(initial)
		if initial.Session != nil || view.session.Text != "No verified Telegram account" {
			t.Errorf("a stored account was shown as signed in: %+v, %q", initial.Session, view.session.Text)
		}
		fynetest.Tap(view.stop)
	})
	transport := &desktopAccountTransport{}
	transport.online.Store(true)
	sessionDone := make(chan error, 1)
	go func() { sessionDone <- engine.RunRecovering(ownerCtx, transport) }()
	defer func() {
		shutdown()
		if err := <-sessionDone; err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("session shutdown: %v", err)
		}
	}()
	stopped := waitStatus(domain.Stopped)
	started, err := control.Call(ctx, dir, app.Command{Operation: app.Start, PublicationID: "news"})
	if err != nil || started.Revision <= stopped.Revision {
		t.Fatalf("console start did not commit: %+v, %v", started, err)
	}
	var active app.Result
	for {
		active = waitStatus(domain.Enabled)
		if active.Session != nil {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("the owner did not expose the currently verified account")
		case <-time.After(5 * time.Millisecond):
		}
	}
	fyne.DoAndWait(func() {
		view.render(active)
		if !strings.Contains(view.details.Text, "enabled") {
			t.Errorf("window did not show console's committed state: %q", view.details.Text)
		}
		if label := view.session.Text; !strings.Contains(label, "Signed in") ||
			!strings.Contains(label, "Fixture Publisher") || !strings.Contains(label, "42") {
			t.Errorf("window did not display the authenticated account: %q", label)
		}
	})
	transport.online.Store(false)
	offline := waitStatus(domain.Enabled)
	fyne.DoAndWait(func() {
		view.render(offline)
		if label := view.session.Text; !strings.Contains(label, "Fixture Publisher") ||
			!strings.Contains(label, "connection unavailable") || strings.Contains(label, "Signed in") {
			t.Errorf("window did not display the unavailable connection separately: %q", label)
		}
		view.Close()
	})
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("closing the window did not stop its loopback owner")
	}
	if _, err := control.Call(ctx, dir, app.Command{Operation: app.Status}); !errors.Is(err, control.ErrAppClosed) {
		t.Fatalf("console still found an owner after the last window closed: %v", err)
	}
}

func TestClosingWindowCancelsQRSignInAtPasswordPrompt(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	application := fynetest.NewApp()
	password := make(chan struct{})
	done := make(chan error, 1)
	view := NewWindow(ctx, application, Backend{SignIn: func(loginCtx context.Context, prompts telegram.SignInPrompts) error {
		if err := prompts.ShowQR(loginCtx, "tg://login?token=local-fixture", time.Now().Add(time.Minute)); err != nil {
			done <- err
			return err
		}
		close(password)
		_, err := prompts.Password(loginCtx)
		done <- err
		return err
	}}, cancel)
	fyne.DoAndWait(view.signIn)
	select {
	case <-password:
	case <-ctx.Done():
		t.Fatal("the QR sign-in did not reach the password prompt")
	}
	fyne.DoAndWait(view.Close)
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("closing the window did not cancel the password prompt: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("the password prompt was left waiting after close")
	}
}

type desktopAccountTransport struct{ online atomic.Bool }

func (f *desktopAccountTransport) Status() telegram.Status {
	return telegram.Status{AccountID: 42, DisplayName: "Fixture Publisher", TransportAvailable: f.online.Load()}
}

func (*desktopAccountTransport) Recover(context.Context, domain.UpdateCheckpoint, []domain.RecoveryReference) (domain.RecoveryBatch, error) {
	return domain.RecoveryBatch{Checkpoint: domain.UpdateCheckpoint{Date: 1_800_000_000}}, nil
}

func (*desktopAccountTransport) ResolveSaved(context.Context, domain.SavedMessage) (content.Kind, error) {
	return "", content.ErrSourceUnavailable
}

func (*desktopAccountTransport) VerifyDestination(context.Context, domain.ChatID, content.Kind) telegram.DestinationCheck {
	return telegram.DestinationCheck{State: telegram.DestinationCheckUnavailable}
}

func (*desktopAccountTransport) PrepareSend(context.Context, domain.ChatID, content.PreparedVariant) (telegram.PreparedSend, telegram.DestinationCheck) {
	return nil, telegram.DestinationCheck{State: telegram.DestinationCheckUnavailable}
}

func TestSavedPreviewRowShowsIdentifyingMediaWithoutACaption(t *testing.T) {
	fynetest.NewApp()
	item := telegram.SavedPreview{
		Reference: domain.SavedMessage{AccountID: 42, MessageID: 17}, Kind: content.Video,
		Media: &telegram.SavedMediaPreview{Filename: "sample.mp4", Width: 1920, Height: 1080, DurationSeconds: 8.5},
	}
	fyne.DoAndWait(func() {
		row := savedPreviewRow(item, func() {})
		var labels []string
		for _, object := range row.Objects {
			if label, ok := object.(*widget.Label); ok {
				labels = append(labels, label.Text)
			}
		}
		for _, expected := range []string{"sample.mp4", "1920 × 1080", "8.5 s"} {
			if !strings.Contains(strings.Join(labels, "\n"), expected) {
				t.Errorf("captionless media fallback lacks %q: %v", expected, labels)
			}
		}
		imageData := []byte("thumbnail delivered by the backend")
		item.Media.InlineThumbnail = imageData
		row = savedPreviewRow(item, func() {})
		for _, object := range row.Objects {
			if image, ok := object.(*canvas.Image); ok && string(image.Resource.Content()) == string(imageData) {
				return
			}
		}
		t.Error("the picker omitted the fetched thumbnail")
	})
}
