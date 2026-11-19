package desktop

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/widget"
	"github.com/google/uuid"

	"telegram-messages-sender/internal/app"
	"telegram-messages-sender/internal/domain"
	"telegram-messages-sender/internal/store"
	"telegram-messages-sender/internal/telegram"
)

func readInterval(text string) (time.Duration, error) {
	interval, err := time.ParseDuration(strings.TrimSpace(text))
	if err != nil || interval <= 0 {
		return 0, errors.New("enter a positive interval such as 20m")
	}
	return interval, nil
}

func (w *Window) changeInterval() {
	id := domain.PublicationID(w.selection.Selected)
	entry := widget.NewEntry()
	for _, item := range w.state.Publications {
		if item.ID == id {
			entry.SetText(item.Interval.String())
			break
		}
	}
	view := dialog.NewCustomWithoutButtons("Change interval", container.NewVBox(widget.NewLabel("Interval"), entry), w.Window)
	view.SetButtons([]fyne.CanvasObject{widget.NewButton("Cancel", view.Hide), widget.NewButton("Save", func() {
		interval, err := readInterval(entry.Text)
		if err != nil {
			dialog.ShowError(err, w.Window)
			return
		}
		w.task(func(ctx context.Context) error {
			_, err := w.api.Command(ctx, app.Command{Operation: app.ChangeInterval, PublicationID: id, Interval: interval})
			return err
		}, func() {
			view.Hide()
			w.Refresh()
		})
	})})
	view.Resize(fyne.NewSize(400, 180))
	view.Show()
}

func (w *Window) editPublication(id domain.PublicationID) {
	var state store.Snapshot
	w.task(func(ctx context.Context) error {
		var err error
		state, err = w.api.Snapshot(ctx)
		return err
	}, func() {
		publication := domain.Publication{State: domain.Stopped, Interval: 20 * time.Minute}
		if id != "" {
			index := slices.IndexFunc(state.Publications, func(item domain.Publication) bool { return item.ID == id })
			if index == -1 {
				dialog.ShowError(app.ErrUnknownPublication, w.Window)
				return
			}
			publication = state.Publications[index]
		}
		w.showEditor(publication, state)
	})
}

func (w *Window) showEditor(publication domain.Publication, snapshot store.Snapshot) {
	id := widget.NewEntry()
	id.SetPlaceHolder("Publication ID")
	id.SetText(string(publication.ID))
	if publication.ID != "" {
		id.Disable()
	}
	interval := widget.NewEntry()
	interval.SetText(publication.Interval.String())
	destinations, variants := container.NewVBox(), container.NewVBox()
	var renderDestinations, renderVariants func()
	titles := make(map[domain.ChatID]string)
	renderDestinations = func() {
		destinations.RemoveAll()
		for i, chat := range publication.Destinations {
			label := string(chat)
			if titles[chat] != "" {
				label = titles[chat] + " · " + label
			}
			up := widget.NewButton("Earlier", func() {
				publication.Destinations[i-1], publication.Destinations[i] = publication.Destinations[i], publication.Destinations[i-1]
				renderDestinations()
			})
			if i == 0 {
				up.Disable()
			}
			remove := widget.NewButton("Remove", func() {
				publication.Destinations = slices.Delete(publication.Destinations, i, i+1)
				renderDestinations()
			})
			destinations.Add(container.NewBorder(nil, nil, nil, container.NewHBox(up, remove), widget.NewLabel(label)))
		}
	}
	renderVariants = func() {
		variants.RemoveAll()
		for i, variant := range publication.Variants {
			name := previewLine(variant.Text)
			edit := widget.NewButton("Edit text", func() {
				w.textVariant(variant.Text, func(text string) {
					publication.Variants[i].Text = text
					renderVariants()
				})
			})
			if variant.SavedMessage != nil {
				name = fmt.Sprintf("Saved Messages · #%d", variant.SavedMessage.MessageID)
				edit.SetText("Replace source")
				edit.OnTapped = func() {
					w.pickSaved(func(ref domain.SavedMessage) {
						publication.Variants[i].SavedMessage = &ref
						renderVariants()
					})
				}
			}
			remove := widget.NewButton("Remove", func() {
				publication.Variants = slices.Delete(publication.Variants, i, i+1)
				renderVariants()
			})
			variants.Add(container.NewBorder(nil, nil, nil, container.NewHBox(edit, remove), widget.NewLabel(name)))
		}
	}
	renderDestinations()
	renderVariants()
	addDestination := widget.NewButton("Add Telegram group", func() {
		w.pickDestination(publication.Destinations, func(item telegram.Destination) {
			titles[item.ID] = item.Title
			publication.Destinations = append(publication.Destinations, item.ID)
			renderDestinations()
		})
	})
	addText := widget.NewButton("Add text", func() {
		w.textVariant("", func(text string) {
			publication.Variants = append(publication.Variants, domain.Variant{ID: domain.VariantID(uuid.NewString()), Text: text})
			renderVariants()
		})
	})
	addSaved := widget.NewButton("Add from Saved Messages", func() {
		w.pickSaved(func(ref domain.SavedMessage) {
			publication.Variants = append(publication.Variants, domain.Variant{ID: domain.VariantID(uuid.NewString()), SavedMessage: &ref})
			renderVariants()
		})
	})
	form := widget.NewForm(widget.NewFormItem("ID", id), widget.NewFormItem("Interval", interval))
	body := container.NewVScroll(container.NewVBox(form, widget.NewSeparator(), widget.NewLabel("Destinations"),
		destinations, addDestination, widget.NewSeparator(), widget.NewLabel("Variants"), variants, container.NewHBox(addText, addSaved)))
	view := dialog.NewCustomWithoutButtons("Edit publication", body, w.Window)
	save := widget.NewButton("Save", func() {
		target := domain.PublicationID(strings.TrimSpace(id.Text))
		if target == "" || (publication.ID == "" && slices.ContainsFunc(snapshot.Publications, func(item domain.Publication) bool {
			return item.ID == target
		})) {
			dialog.ShowError(errors.New("enter a unique publication ID"), w.Window)
			return
		}
		cadence, err := readInterval(interval.Text)
		if err != nil {
			dialog.ShowError(err, w.Window)
			return
		}
		value := publication
		value.ID, value.Interval = target, cadence
		value.Destinations = slices.Clone(publication.Destinations)
		value.Variants = slices.Clone(publication.Variants)
		w.task(func(ctx context.Context) error {
			_, err := w.api.Save(ctx, snapshot.Revision, value)
			return err
		}, func() {
			view.Hide()
			w.Refresh()
		})
	})
	view.SetButtons([]fyne.CanvasObject{widget.NewButton("Cancel", view.Hide), save})
	view.Resize(fyne.NewSize(650, 420))
	view.Show()
}

func (w *Window) textVariant(text string, done func(string)) {
	entry := widget.NewMultiLineEntry()
	entry.SetMinRowsVisible(5)
	entry.SetText(text)
	view := dialog.NewCustomWithoutButtons("Manual text", entry, w.Window)
	view.SetButtons([]fyne.CanvasObject{widget.NewButton("Cancel", view.Hide), widget.NewButton("Use text", func() {
		if strings.TrimSpace(entry.Text) == "" {
			dialog.ShowError(errors.New("enter the text to publish"), w.Window)
			return
		}
		done(entry.Text)
		view.Hide()
	})})
	view.Resize(fyne.NewSize(490, 250))
	view.Show()
}

func (w *Window) pickDestination(existing []domain.ChatID, done func(telegram.Destination)) {
	var items []telegram.Destination
	w.task(func(ctx context.Context) error {
		var err error
		items, err = w.api.Destinations(ctx)
		return err
	}, func() {
		rows := container.NewVBox()
		var view *dialog.CustomDialog
		for _, item := range items {
			if !slices.Contains(existing, item.ID) {
				rows.Add(widget.NewButton(fmt.Sprintf("%s · %s", item.Title, item.ID), func() {
					done(item)
					view.Hide()
				}))
			}
		}
		if len(rows.Objects) == 0 {
			rows.Add(widget.NewLabel("No other Telegram groups available"))
		}
		view = dialog.NewCustom("Select destination", "Cancel", container.NewVScroll(rows), w.Window)
		view.Resize(fyne.NewSize(510, 385))
		view.Show()
	})
}

func (w *Window) pickSaved(done func(domain.SavedMessage)) {
	rows := container.NewVBox()
	var offsets = []int{0}
	var nextOffset int
	previous, next := widget.NewButton("Previous", nil), widget.NewButton("Next", nil)
	previous.Disable()
	next.Disable()
	view := dialog.NewCustom("Saved Messages", "Cancel", container.NewBorder(nil, container.NewHBox(previous, next), nil, nil, container.NewVScroll(rows)), w.Window)
	pickerCtx, cancel := context.WithCancel(w.ctx)
	view.SetOnClosed(cancel)
	var load func([]int)
	load = func(newOffsets []int) {
		previous.Disable()
		next.Disable()
		var page telegram.SavedPage
		var lookupErr error
		w.task(func(context.Context) error {
			ctx, stop := context.WithTimeout(pickerCtx, 25*time.Second)
			defer stop()
			page, lookupErr = w.api.ListSaved(ctx, newOffsets[len(newOffsets)-1], 20)
			return nil
		}, func() {
			if pickerCtx.Err() != nil {
				return
			}
			rows.RemoveAll()
			if lookupErr != nil {
				rows.Add(widget.NewLabel(lookupErr.Error()))
				rows.Add(widget.NewButton("Retry", func() {
					rows.RemoveAll()
					load(newOffsets)
				}))
				return
			}
			offsets, nextOffset = newOffsets, page.NextOffset
			for _, item := range page.Items {
				if item.Unavailable != nil {
					rows.Add(widget.NewLabel(fmt.Sprintf("#%d · %s", item.Reference.MessageID, item.Unavailable)))
					continue
				}
				rows.Add(savedPreviewRow(item, func() {
					done(item.Reference)
					view.Hide()
				}))
			}
			if len(page.Items) == 0 {
				rows.Add(widget.NewLabel("No messages on this page"))
			}
			previous.Disable()
			next.Disable()
			if len(offsets) > 1 {
				previous.Enable()
			}
			if nextOffset != 0 {
				next.Enable()
			}
		})
	}
	previous.OnTapped = func() { load(slices.Clone(offsets[:len(offsets)-1])) }
	next.OnTapped = func() { load(append(slices.Clone(offsets), nextOffset)) }
	view.Resize(fyne.NewSize(575, 460))
	view.Show()
	load(offsets)
}

func savedPreviewRow(item telegram.SavedPreview, use func()) *fyne.Container {
	label := fmt.Sprintf("#%d · %s · %s", item.Reference.MessageID, item.Kind, item.SentAt.Local().Format(time.DateTime))
	row := container.NewVBox(widget.NewLabel(label))
	if item.Text != "" {
		row.Add(widget.NewLabel(previewLine(item.Text)))
	}
	if media := item.Media; media != nil {
		if details := savedMediaDescription(media); details != "" {
			row.Add(widget.NewLabel(details))
		}
		if len(media.InlineThumbnail) != 0 {
			photo := canvas.NewImageFromResource(fyne.NewStaticResource(fmt.Sprintf("saved-%d", item.Reference.MessageID), media.InlineThumbnail))
			photo.SetMinSize(fyne.NewSize(100, 70))
			photo.FillMode = canvas.ImageFillContain
			row.Add(photo)
		}
	}
	row.Add(widget.NewButton("Use message", use))
	return row
}

func savedMediaDescription(media *telegram.SavedMediaPreview) string {
	var details []string
	if media.Filename != "" {
		details = append(details, previewLine(media.Filename))
	}
	if media.Width > 0 && media.Height > 0 {
		details = append(details, fmt.Sprintf("%d × %d", media.Width, media.Height))
	}
	if media.DurationSeconds > 0 {
		details = append(details, fmt.Sprintf("%g s", media.DurationSeconds))
	}
	if media.Spoiler {
		details = append(details, "Spoiler")
	}
	return strings.Join(details, " · ")
}

func previewLine(value string) string {
	text := []rune(strings.Join(strings.Fields(value), " "))
	if len(text) > 85 {
		return string(text[:85]) + "…"
	}
	return string(text)
}
