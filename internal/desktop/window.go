package desktop

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	fynedesktop "fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"telegram-messages-sender/internal/app"
	"telegram-messages-sender/internal/domain"
	"telegram-messages-sender/internal/store"
	"telegram-messages-sender/internal/telegram"
)

type Backend struct {
	Command          app.Handler
	SetAllowNonAdmin func(context.Context, bool) error
	Snapshot         func(context.Context) (store.Snapshot, error)
	Save             func(context.Context, int64, domain.Publication) (int64, error)
	ListSaved        func(context.Context, int, int) (telegram.SavedPage, error)
	Destinations     func(context.Context) ([]telegram.Destination, error)
	SignIn           func(context.Context, telegram.SignInPrompts) error
	SignOut          func(ctx context.Context, localOnly bool) error
	Credentials      func(context.Context, int, string) error
}

type Window struct {
	fyne.Window
	ctx           context.Context
	api           Backend
	selection     *widget.Select
	start         *widget.Button
	stop          *widget.Button
	interval      *widget.Button
	edit          *widget.Button
	signInButton  *widget.Button
	signOutButton *widget.Button
	allowNonAdmin *widget.Check
	details       *widget.Label
	session       *widget.Label
	notice        *widget.Label
	chats         *fyne.Container
	state         app.Result
	loading       bool
}

func NewWindow(ctx context.Context, application fyne.App, backend Backend, shutdown func()) *Window {
	view := &Window{Window: application.NewWindow("Telegram Messages Sender"), ctx: ctx, api: backend}
	view.SetMaster()
	view.SetOnClosed(shutdown)
	view.SetCloseIntercept(func() {
		shutdown()
		view.SetCloseIntercept(nil)
		view.Close()
	})
	view.Canvas().AddShortcut(&fynedesktop.CustomShortcut{KeyName: fyne.KeyW, Modifier: fyne.KeyModifierSuper},
		func(fyne.Shortcut) { view.Close() })
	quit := fyne.NewMenuItem("Quit", view.Close)
	quit.IsQuit = true
	view.SetMainMenu(fyne.NewMainMenu(fyne.NewMenu("Application", quit)))

	view.selection = widget.NewSelect(nil, func(string) { view.showSelection() })
	view.selection.PlaceHolder = "Select a publication"
	view.start = widget.NewButton("Start", func() { view.publicationCommand(app.Start) })
	view.stop = widget.NewButton("Stop", func() { view.publicationCommand(app.Stop) })
	view.interval = widget.NewButton("Change interval", view.changeInterval)
	view.edit = widget.NewButton("Edit", func() { view.editPublication(domain.PublicationID(view.selection.Selected)) })
	view.details = widget.NewLabel("")
	view.details.Wrapping = fyne.TextWrapWord
	view.session = widget.NewLabel(sessionText(nil))
	view.session.Wrapping = fyne.TextWrapWord
	view.notice = widget.NewLabel("")
	view.notice.Wrapping = fyne.TextWrapWord
	view.notice.Hide()
	view.chats = container.NewVBox()
	view.allowNonAdmin = widget.NewCheck("Allow sending without administrator rights", view.changeAllowNonAdmin)
	if backend.SetAllowNonAdmin == nil {
		view.allowNonAdmin.Disable()
	}
	view.signInButton = widget.NewButton("Sign in", view.signIn)
	view.signOutButton = widget.NewButton("Sign out", view.signOut)
	if backend.SignOut == nil {
		view.signOutButton.Disable()
	}

	header := widget.NewLabelWithStyle("Publications", fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
	actionBar := container.NewHBox(
		widget.NewButtonWithIcon("New publication", theme.ContentAddIcon(), func() { view.editPublication("") }),
		widget.NewButtonWithIcon("Refresh", theme.ViewRefreshIcon(), view.Refresh),
		view.signInButton,
		view.signOutButton,
		widget.NewButton("API credentials", view.credentials),
	)
	publicationBar := container.NewHBox(view.start, view.stop, view.interval, view.edit)
	content := container.NewVBox(header, view.session, view.notice, actionBar, view.allowNonAdmin, widget.NewSeparator(), view.selection, publicationBar, view.details, view.chats)
	view.SetContent(container.NewPadded(container.NewVScroll(content)))
	view.Resize(fyne.NewSize(780, 560))
	view.showSelection()
	return view
}

func (w *Window) Watch() {
	w.Refresh()
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-w.ctx.Done():
				return
			case <-ticker.C:
				fyne.Do(w.Refresh)
			}
		}
	}()
}

func (w *Window) Refresh() {
	if w.loading || w.ctx.Err() != nil {
		return
	}
	w.loading = true
	go func() {
		ctx, cancel := context.WithTimeout(w.ctx, 5*time.Second)
		defer cancel()
		state, err := w.api.Command(ctx, app.Command{Operation: app.Status})
		fyne.Do(func() {
			w.loading = false
			if w.ctx.Err() != nil {
				return
			}
			if err != nil {
				w.showNotice(err.Error())
				return
			}
			w.render(state)
		})
	}()
}

func (w *Window) render(state app.Result) {
	chatsChanged := !slices.Equal(w.state.DisabledChats, state.DisabledChats)
	w.state = state
	if !w.allowNonAdmin.Disabled() {
		w.allowNonAdmin.SetChecked(state.AllowNonAdmin)
	}
	setLabelText(w.session, sessionText(state.Session))
	w.showNotice(state.DispatchError)
	options := make([]string, 0, len(state.Publications))
	for _, item := range state.Publications {
		options = append(options, string(item.ID))
	}
	if !slices.Equal(w.selection.Options, options) {
		w.selection.SetOptions(options)
	}
	if !slices.Contains(options, w.selection.Selected) {
		if len(options) != 0 {
			w.selection.SetSelected(options[0])
		} else if w.selection.Selected != "" {
			w.selection.ClearSelected()
		}
	}
	if chatsChanged {
		rows := make([]fyne.CanvasObject, 0, len(state.DisabledChats))
		for _, disabled := range state.DisabledChats {
			id := disabled.ID
			rows = append(rows, container.NewBorder(nil, nil, nil, widget.NewButton("Re-enable", func() {
				w.command(app.Command{Operation: app.ReenableChat, ChatID: id})
			}), widget.NewLabel(fmt.Sprintf("%s · chat disabled", id))))
		}
		w.chats.Objects = rows
		w.chats.Refresh()
	}
	w.showSelection()
}

func (w *Window) changeAllowNonAdmin(allow bool) {
	if w.allowNonAdmin.Disabled() || allow == w.state.AllowNonAdmin {
		return
	}
	w.allowNonAdmin.Disable()
	w.task(func(ctx context.Context) error {
		err := w.api.SetAllowNonAdmin(ctx, allow)
		fyne.Do(func() {
			if w.ctx.Err() != nil {
				return
			}
			if err == nil {
				w.state.AllowNonAdmin = allow
			}
			w.allowNonAdmin.SetChecked(w.state.AllowNonAdmin)
			w.allowNonAdmin.Enable()
			w.Refresh()
		})
		return err
	}, nil)
}

func sessionText(session *app.SessionStatus) string {
	if session == nil {
		return "No verified Telegram account"
	}
	account := fmt.Sprintf("Telegram account %d", session.AccountID)
	if session.DisplayName != "" {
		account = session.DisplayName + " · " + account
	}
	if !session.TransportAvailable {
		return account + " · connection unavailable"
	}
	return "Signed in · " + account
}

func (w *Window) showNotice(message string) {
	setLabelText(w.notice, message)
	if message == "" {
		w.notice.Hide()
	} else {
		w.notice.Show()
	}
}

func (w *Window) showSelection() {
	enabled := w.selection.Selected != ""
	for _, button := range []*widget.Button{w.start, w.stop, w.interval, w.edit} {
		if enabled {
			button.Enable()
		} else {
			button.Disable()
		}
	}
	var lines []string
	for _, publication := range w.state.Publications {
		if string(publication.ID) != w.selection.Selected {
			continue
		}
		lines = append(lines, fmt.Sprintf("%s · every %s", statusText(string(publication.State), string(publication.Reason)), publication.Interval))
	}
	for _, wait := range w.state.Waits {
		label := string(wait.ChatID)
		if label == "" {
			label = "Account"
		}
		lines = append(lines, fmt.Sprintf("%s · Telegram wait until %s", label, wait.Until.Local().Format(time.DateTime)))
	}
	for _, item := range w.state.Dispatch {
		if string(item.PublicationID) != w.selection.Selected {
			continue
		}
		label := fmt.Sprintf("%s · %s", item.ChatID, statusText(string(item.Outcome), item.Reason))
		if item.DueAt != nil {
			label += " · due " + item.DueAt.Local().Format(time.DateTime)
		}
		if item.Delay > 0 {
			label += " · delayed " + item.Delay.Round(time.Second).String()
		}
		if item.Unresolved > 0 {
			label += fmt.Sprintf(" · unresolved attempts %d", item.Unresolved)
		}
		lines = append(lines, label)
	}
	setLabelText(w.details, strings.Join(lines, "\n"))
}

func setLabelText(label *widget.Label, text string) {
	if label.Text != text {
		label.SetText(text)
	}
}

func statusText(state, reason string) string {
	switch reason {
	case "manual_stop":
		return "Stopped"
	case "variants_exhausted":
		return "Paused · no available variants"
	case "check_unavailable":
		return "Destination verification unavailable"
	case "admin_required":
		return "Administrator or owner rights required"
	case "source_unavailable":
		return "Saved Messages source unavailable"
	case "publishing_restricted":
		return "Publishing restricted"
	case "personal_identity_unavailable":
		return "Personal sender unavailable"
	case "telegram_wait":
		return "Telegram wait"
	case "telegram_rejected":
		return "Telegram rejected the send"
	case "no_delivery_confirmation":
		if state == string(domain.AttemptUnknown) {
			return "Delivery unknown"
		}
	}
	if state == "" {
		return "Scheduled"
	}
	if state == string(domain.AttemptUnknown) {
		return "Delivery unknown"
	}
	if reason != "" {
		return strings.ReplaceAll(reason, "_", " ")
	}
	return strings.ReplaceAll(state, "_", " ")
}

func (w *Window) publicationCommand(operation app.Operation) {
	w.command(app.Command{Operation: operation, PublicationID: domain.PublicationID(w.selection.Selected)})
}

func (w *Window) command(command app.Command) {
	w.task(func(ctx context.Context) error {
		_, err := w.api.Command(ctx, command)
		return err
	}, w.Refresh)
}

func (w *Window) task(work func(context.Context) error, success func()) {
	go func() {
		ctx, cancel := context.WithTimeout(w.ctx, 25*time.Second)
		defer cancel()
		err := work(ctx)
		fyne.Do(func() {
			if w.ctx.Err() != nil {
				return
			}
			if err != nil {
				dialog.ShowError(err, w.Window)
			} else if success != nil {
				success()
			}
		})
	}()
}
