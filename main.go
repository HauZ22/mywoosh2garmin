// Command mywhoosh2garmin is the desktop GUI: it lists your MyWhoosh rides and
// uploads them to Garmin Connect. All sync logic lives in internal/core and is
// shared with the headless server (cmd/server).
package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"mywhoosh2garmin/internal/core"
)

// version is set at build time via -ldflags "-X main.version=...".
var version = "dev"

func appConfigDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".mywhoosh2garmin")
}

func main() {
	a := app.New()
	title := "MyWhoosh2Garmin"
	if version != "dev" {
		title += " " + version
	}
	w := a.NewWindow(title)
	w.Resize(fyne.NewSize(760, 720))

	dir := appConfigDir()
	cfg := core.LoadConfig(dir)
	syncer := core.New(dir, cfg)

	// --- Log panel ---
	logEntry := widget.NewMultiLineEntry()
	logEntry.Wrapping = fyne.TextWrapWord
	logEntry.Disable()
	logScroll := container.NewVScroll(logEntry)
	logScroll.SetMinSize(fyne.NewSize(0, 140))

	var logMu sync.Mutex
	var logText string

	appendLog := func(msg string) {
		logMu.Lock()
		logText += msg + "\n"
		text := logText
		logMu.Unlock()
		fyne.Do(func() {
			logEntry.Enable()
			logEntry.SetText(text)
			logEntry.CursorRow = len(strings.Split(text, "\n"))
			logEntry.Disable()
			logScroll.ScrollToBottom()
		})
	}
	syncer.SetLog(func(format string, args ...interface{}) {
		appendLog(fmt.Sprintf(format, args...))
	})

	// --- Credentials ---
	mwEmailEntry := widget.NewEntry()
	mwEmailEntry.SetPlaceHolder("MyWhoosh email")
	mwEmailEntry.SetText(cfg.MyWhooshEmail)

	mwPasswordEntry := widget.NewPasswordEntry()
	mwPasswordEntry.SetPlaceHolder("MyWhoosh password (only needed first time)")

	garminEmailEntry := widget.NewEntry()
	garminEmailEntry.SetPlaceHolder("Garmin email")
	garminEmailEntry.SetText(cfg.GarminEmail)

	garminPasswordEntry := widget.NewPasswordEntry()
	garminPasswordEntry.SetPlaceHolder("Garmin password (only needed first time)")

	// applyCredentials hands the entered accounts to the syncer. Only the e-mail
	// addresses are written to disk; passwords stay in memory.
	applyCredentials := func() {
		c := syncer.Config()
		c.MyWhooshEmail, c.MyWhooshPassword = mwEmailEntry.Text, mwPasswordEntry.Text
		c.GarminEmail, c.GarminPassword = garminEmailEntry.Text, garminPasswordEntry.Text
		syncer.ApplyConfig(c)

		c.MyWhooshPassword, c.GarminPassword = "", ""
		if err := core.SaveConfig(dir, c); err != nil {
			appendLog("⚠ Konfiguration konnte nicht gespeichert werden: " + err.Error())
		}
	}

	// --- Activity list ---
	activityListBox := container.NewVBox()
	activityScroll := container.NewVScroll(activityListBox)
	activityScroll.SetMinSize(fyne.NewSize(0, 280))

	var (
		activities []core.ActivityDisplayItem // UI thread only
		busy       bool                       // UI thread only
	)

	uploadAllBtn := widget.NewButton("⬆  Upload All to Garmin", nil)
	uploadAllBtn.Importance = widget.HighImportance
	uploadAllBtn.Disable()
	markAllBtn := widget.NewButtonWithIcon("Mark all open as uploaded", theme.ConfirmIcon(), nil)
	markAllBtn.Disable()
	fetchBtn := widget.NewButton("📋  Fetch Activities", nil)
	fetchBtn.Importance = widget.HighImportance

	unsyncedCount := func() int {
		n := 0
		for _, it := range activities {
			if !it.IsSynced {
				n++
			}
		}
		return n
	}

	// refreshSyncState re-reads the persistent markers into the visible list.
	refreshSyncState := func() {
		for i := range activities {
			e, ok := syncer.Tracker().Entry(activities[i].ID)
			activities[i].IsSynced = ok
			activities[i].SyncSource = e.Source
		}
	}

	updateBulkButtons := func() {
		if n := unsyncedCount(); n > 0 && !busy {
			uploadAllBtn.SetText(fmt.Sprintf("⬆  Upload All to Garmin (%d new)", n))
			uploadAllBtn.Enable()
			markAllBtn.Enable()
		} else {
			if n == 0 && len(activities) > 0 {
				uploadAllBtn.SetText("✓  All synced")
			} else if len(activities) == 0 {
				uploadAllBtn.SetText("⬆  Upload All to Garmin")
			}
			uploadAllBtn.Disable()
			markAllBtn.Disable()
		}
	}

	// runBusy runs job in the background and keeps the UI consistent: buttons are
	// locked while it runs and the list is redrawn from the persistent state after.
	var renderList func()
	runBusy := func(job func()) {
		if busy {
			return
		}
		busy = true
		fetchBtn.Disable()
		updateBulkButtons()
		go func() {
			defer fyne.Do(func() {
				busy = false
				fetchBtn.Enable()
				refreshSyncState()
				renderList()
			})
			job()
		}()
	}

	syncLabel := func(it core.ActivityDisplayItem) string {
		switch it.SyncSource {
		case core.SourceManual:
			return "✓ marked as uploaded (manual)"
		case core.SourceDuplicate:
			return "✓ already on Garmin"
		case core.SourceAutoSkip:
			return "✓ skipped (older than auto-sync)"
		default:
			return "✓ uploaded"
		}
	}

	buildRow := func(it core.ActivityDisplayItem) fyne.CanvasObject {
		details := it.FormattedDate() + "  —  " + it.DisplayName()
		if v := it.FormattedDistance(); v != "" {
			details += "  •  " + v
		}
		if v := it.FormattedDuration(); v != "" {
			details += "  •  " + v
		}
		if it.AvgPower > 0 {
			details += fmt.Sprintf("  •  %.0fW", it.AvgPower)
		}
		if it.AvgHR > 0 {
			details += fmt.Sprintf("  •  %.0fbpm", it.AvgHR)
		}
		infoLabel := widget.NewLabel(details)
		infoLabel.Wrapping = fyne.TextWrapWord

		id := it.ID
		var buttons *fyne.Container
		if it.IsSynced {
			undoBtn := widget.NewButtonWithIcon("Undo", theme.ContentUndoIcon(), func() {
				if busy {
					return
				}
				if _, err := syncer.SetActivitiesSynced([]string{id}, false); err != nil {
					appendLog("⚠ " + err.Error())
				}
				refreshSyncState()
				renderList()
			})
			buttons = container.NewHBox(widget.NewLabel(syncLabel(it)), undoBtn)
		} else {
			act := it.Activity
			uploadBtn := widget.NewButtonWithIcon("Upload", theme.UploadIcon(), func() {
				applyCredentials()
				runBusy(func() { _, _ = syncer.SyncActivity(act) })
			})
			uploadBtn.Importance = widget.HighImportance
			markBtn := widget.NewButtonWithIcon("Already on Garmin", theme.ConfirmIcon(), func() {
				if busy {
					return
				}
				if _, err := syncer.SetActivitiesSynced([]string{id}, true); err != nil {
					appendLog("⚠ " + err.Error())
				}
				refreshSyncState()
				renderList()
			})
			buttons = container.NewHBox(uploadBtn, markBtn)
		}
		return container.NewBorder(nil, nil, nil, buttons, infoLabel)
	}

	renderList = func() {
		activityListBox.RemoveAll()
		if len(activities) == 0 {
			activityListBox.Add(widget.NewLabel("No activities loaded."))
		}
		for _, it := range activities {
			activityListBox.Add(buildRow(it))
		}
		activityListBox.Refresh()
		updateBulkButtons()
	}

	fetchBtn.OnTapped = func() {
		applyCredentials()
		runBusy(func() {
			items, err := syncer.ListActivities(0)
			if err != nil {
				appendLog("❌ " + err.Error())
				return
			}
			appendLog(fmt.Sprintf("✓ %d activities loaded", len(items)))
			fyne.Do(func() { activities = items })
		})
	}

	uploadAllBtn.OnTapped = func() {
		if busy {
			return
		}
		applyCredentials()
		var todo []core.ActivityDisplayItem
		for _, it := range activities {
			if !it.IsSynced {
				todo = append(todo, it)
			}
		}
		runBusy(func() {
			uploaded, failed := 0, 0
			for i, it := range todo {
				appendLog(fmt.Sprintf("\n[%d/%d] %s", i+1, len(todo), it.DisplayName()))
				res, err := syncer.SyncActivity(it.Activity)
				switch res.Status {
				case core.StatusUploaded, core.StatusDuplicate:
					uploaded++
				case core.StatusFailed:
					failed++
				}
				if errors.Is(err, core.ErrGarminAuth) {
					appendLog("❌ Garmin login failed — stopping")
					break
				}
			}
			appendLog(fmt.Sprintf("\n✓ Batch complete — %d uploaded, %d failed", uploaded, failed))
		})
	}

	markAllBtn.OnTapped = func() {
		if busy {
			return
		}
		var ids []string
		for _, it := range activities {
			if !it.IsSynced {
				ids = append(ids, it.ID)
			}
		}
		msg := fmt.Sprintf("Mark %d open activities as already uploaded to Garmin?\nThey will never be uploaded by this app (you can undo it per activity).", len(ids))
		dialog.ShowConfirm("Mark as uploaded", msg, func(ok bool) {
			if !ok {
				return
			}
			if _, err := syncer.SetActivitiesSynced(ids, true); err != nil {
				appendLog("⚠ " + err.Error())
			}
			refreshSyncState()
			renderList()
		}, w)
	}

	// --- Layout ---
	header := widget.NewRichTextFromMarkdown("## MyWhoosh → Garmin")

	mywhooshSection := container.NewVBox(widget.NewLabel("MyWhoosh Account"), mwEmailEntry, mwPasswordEntry)
	garminSection := container.NewVBox(widget.NewLabel("Garmin Connect"), garminEmailEntry, garminPasswordEntry)
	credentialsRow := container.New(layout.NewGridWrapLayout(fyne.NewSize(350, 130)), mywhooshSection, garminSection)

	activitiesHeader := container.NewBorder(nil, nil,
		widget.NewRichTextFromMarkdown(fmt.Sprintf("### Activities (last %d days)", cfg.EffectiveDays())),
		container.NewHBox(markAllBtn, uploadAllBtn),
	)

	topForm := container.NewVBox(
		header,
		widget.NewSeparator(),
		credentialsRow,
		widget.NewSeparator(),
		fetchBtn,
		widget.NewSeparator(),
		activitiesHeader,
	)

	w.SetContent(container.NewBorder(topForm, logScroll, nil, nil, activityScroll))
	renderList()
	w.ShowAndRun()
}
