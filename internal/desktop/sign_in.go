package desktop

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/widget"
	"github.com/skip2/go-qrcode"

	"telegram-messages-sender/internal/telegram"
)

func (w *Window) signIn() {
	ctx, cancel := context.WithCancel(w.ctx)
	description := widget.NewLabel("Connecting to Telegram…")
	code := canvas.NewImageFromImage(nil)
	code.SetMinSize(fyne.NewSize(240, 240))
	code.FillMode = canvas.ImageFillContain
	code.Hide()
	password := widget.NewPasswordEntry()
	password.SetPlaceHolder("Telegram two-step password")
	passwords := make(chan string, 1)
	submit := widget.NewButton("Verify", func() {
		if password.Text == "" {
			return
		}
		select {
		case passwords <- password.Text:
			password.SetText("")
		default:
		}
	})
	passwordPanel := container.NewVBox(password, submit)
	passwordPanel.Hide()
	view := dialog.NewCustom("Telegram sign-in", "Cancel", container.NewVBox(description, code, passwordPanel), w.Window)
	view.SetOnClosed(cancel)
	password.OnSubmitted = func(string) { submit.OnTapped() }
	view.Resize(fyne.NewSize(380, 395))
	view.Show()

	prompts := telegram.SignInPrompts{
		ShowQR: func(promptCtx context.Context, loginURL string, expires time.Time) error {
			qr, err := qrcode.New(loginURL, qrcode.Medium)
			if err != nil {
				return telegram.ErrQRDisplay
			}
			image := qr.Image(256)
			fyne.Do(func() {
				if promptCtx.Err() == nil && ctx.Err() == nil {
					description.SetText("Scan with Telegram · expires " + expires.Local().Format(time.TimeOnly))
					code.Image = image
					code.Refresh()
					code.Show()
				}
			})
			return promptCtx.Err()
		},
		Password: func(promptCtx context.Context) (string, error) {
			fyne.Do(func() {
				if ctx.Err() == nil && promptCtx.Err() == nil {
					code.Hide()
					description.SetText("Enter your Telegram two-step verification password")
					passwordPanel.Show()
					w.Canvas().Focus(password)
				}
			})
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-promptCtx.Done():
				return "", promptCtx.Err()
			case value := <-passwords:
				return value, nil
			}
		},
	}
	go func() {
		err := w.api.SignIn(ctx, prompts)
		fyne.Do(func() {
			if ctx.Err() != nil {
				return
			}
			password.SetText("")
			code.Image = nil
			view.Hide()
			if err != nil && !errors.Is(err, context.Canceled) {
				dialog.ShowError(err, w.Window)
			}
			w.Refresh()
		})
	}()
}

func (w *Window) credentials() {
	id, hash := widget.NewEntry(), widget.NewPasswordEntry()
	view := dialog.NewCustomWithoutButtons("Telegram API credentials",
		widget.NewForm(widget.NewFormItem("API ID", id), widget.NewFormItem("API hash", hash)), w.Window)
	view.SetButtons([]fyne.CanvasObject{widget.NewButton("Cancel", view.Hide), widget.NewButton("Save to Keychain", func() {
		appID, err := strconv.ParseInt(strings.TrimSpace(id.Text), 10, 32)
		appHash := strings.TrimSpace(hash.Text)
		if err != nil || appID <= 0 || appHash == "" {
			dialog.ShowError(errors.New("enter a valid Telegram API ID and hash"), w.Window)
			return
		}
		w.task(func(ctx context.Context) error {
			return w.api.Credentials(ctx, int(appID), appHash)
		}, func() {
			hash.SetText("")
			view.Hide()
			w.Refresh()
		})
	})})
	view.SetOnClosed(func() { hash.SetText("") })
	view.Resize(fyne.NewSize(420, 180))
	view.Show()
}
