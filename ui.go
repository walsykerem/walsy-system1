package main

import (
	"bytes"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// This file is the entire client layer: plain forms and server-rendered pages.
// No external assets and no client-side state; two small inline helpers only
// (Escape closes a sheet, the combo search filters the rendered list).

type uiStep struct {
	Index  int
	Number int
	Label  string
	First  bool
	Last   bool
}

type uiOption struct {
	Provider string
	Model    string
	Active   bool
}

type uiPage struct {
	Title string
	// Msg/MsgMs are the frontend toast contract: Msg is the flash text and
	// MsgMs its lifetime in milliseconds.
	Msg         string
	MsgMs       int
	Name        string
	Models      []string
	Providers   []dashboardProvider
	Combo       []uiStep
	Options     []uiOption
	Search      string
	Winner      string
	Results     []ComboResult
	KeyCount    int
	KeySlots    []int
	BuildTarget string
	BuildStatus string
	// Standalone provider sheet fields mirror dashboardProvider so the shared
	// providerKeys template renders with a flat uiPage too.
	Base        string
	DefaultBase string
	DocsURL     string
	CanReset    bool
	Demo        bool
	KeySet      bool
	// Add-provider form echo: a refused save keeps what was typed and the
	// details element open instead of wiping the form.
	AddOpen   bool
	AddName   string
	AddBase   string
	AddKeyEnv string
	// SheetMode renders the standalone provider iframe document.
	// ComboSheetMode renders the inner combo form; ComboResultMode its result;
	// ComboAddMode the inner add-to-combo form. All are false on the home
	// document.
	SheetMode       bool
	ComboSheetMode  bool
	ComboResultMode bool
	ComboAddMode    bool
	// LogsSheetMode renders the inner LOGS sheet with its call ring.
	LogsSheetMode bool
	Log           LogTotals
	Logs          []LogEntry
	// RefreshParent tells the sheet document to ask its parent to reload after
	// an in-sheet write, so the home document drops stale counts and combo.
	RefreshParent bool
	// MsgOK marks a toast that follows a successful write, so a refusal and a
	// confirmation do not look identical.
	MsgOK bool
}

var uiTemplate = template.Must(template.New("index.html").ParseFS(dashboardAssets, "web/index.html"))

const (
	defaultMsgMs = 6000
	minMsgMs     = 1000
	maxMsgMs     = 30000
)

// uiMsgMs reads the optional ms query parameter and clamps it so a crafted URL
// can neither pin a toast forever nor flash it invisible.
func uiMsgMs(r *http.Request) int {
	raw := r.URL.Query().Get("ms")
	if raw == "" {
		return defaultMsgMs
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return defaultMsgMs
	}
	if n < minMsgMs {
		return minMsgMs
	}
	if n > maxMsgMs {
		return maxMsgMs
	}
	return n
}

// uiWriteMessages lists the toasts that follow a persisted write. Every other
// sheet message (rejection, key test) is read-only and must not reload the home.
var uiWriteMessages = map[string]bool{
	"KEY ADDED":        true,
	"KEY REMOVED":      true,
	"KEYS CLEARED":     true,
	"MODEL ADDED":      true,
	"MODEL REMOVED":    true,
	"ENDPOINT SAVED":   true,
	"ENDPOINT RESET":   true,
	"PROVIDER SAVED":   true,
	"PROVIDER REMOVED": true,
}

func uiWroteState(msg string) bool {
	return uiWriteMessages[msg]
}

func renderPage(w http.ResponseWriter, page uiPage) {
	if page.MsgMs == 0 {
		page.MsgMs = defaultMsgMs
	}
	// A sheet that just wrote state must refresh the home document: only the
	// iframe navigated, so without this the parent keeps stale rows. A refusal
	// or a key-test result must stay on screen instead of being reloaded away.
	page.MsgOK = uiWroteState(page.Msg)
	page.RefreshParent = page.MsgOK &&
		(page.SheetMode || page.ComboSheetMode || page.ComboResultMode || page.ComboAddMode)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Sheets re-render the same URLs; a cached document hides freshly added state.
	w.Header().Set("Cache-Control", "no-store")
	// Render aside first: a template error after the body started would make
	// the handler write a second header on a half-written page.
	var buf bytes.Buffer
	if err := uiTemplate.ExecuteTemplate(&buf, "index.html", page); err != nil {
		http.Error(w, "render failed", http.StatusInternalServerError)
		return
	}
	_, _ = buf.WriteTo(w)
}

func uiGo(w http.ResponseWriter, r *http.Request, path, msg string) {
	if msg != "" {
		fragment := ""
		if i := strings.IndexByte(path, '#'); i >= 0 {
			fragment, path = path[i:], path[:i]
		}
		sep := "?"
		if strings.Contains(path, "?") {
			sep = "&"
		}
		path += sep + "msg=" + url.QueryEscape(msg) + fragment
	}
	http.Redirect(w, r, path, http.StatusSeeOther)
}

// sheetTarget is the post-action redirect for every form inside the provider
// sheet iframe, so only the iframe navigates and the home document stays put.
func sheetTarget(name string) string {
	return "/ui/providers/sheet?name=" + url.QueryEscape(name)
}

func uiForm(w http.ResponseWriter, r *http.Request) (url.Values, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
	if err := r.ParseForm(); err != nil {
		return nil, false
	}
	return r.PostForm, true
}

func uiSteps(steps []stateStep) []uiStep {
	out := make([]uiStep, 0, len(steps))
	for i, s := range steps {
		label := s.Provider
		if s.Model != "" {
			label += "/" + s.Model
		}
		out = append(out, uiStep{
			Index:  i,
			Number: i + 1,
			Label:  label,
			First:  i == 0,
			Last:   i == len(steps)-1,
		})
	}
	return out
}

func uiHomePage(msg string) uiPage {
	goos, goarch := effectiveBuildTarget()
	p := uiPage{Title: "Providers / Combo", Msg: msg, BuildTarget: goos + "/" + goarch}
	view, err := providerView()
	if err != nil {
		p.Msg = msg + " STATE UNAVAILABLE"
		return p
	}
	p.Providers = view.Providers
	steps, err := loadCombo()
	if err != nil {
		p.Msg = msg + " STATE UNAVAILABLE"
		return p
	}
	p.Combo = uiSteps(steps)
	return p
}

func uiHome(r *http.Request) uiPage {
	p := uiHomePage(r.URL.Query().Get("msg"))
	p.MsgMs = uiMsgMs(r)
	return p
}

func uiMain(w http.ResponseWriter, r *http.Request) {
	renderPage(w, uiHome(r))
}

func uiAddProvider(w http.ResponseWriter, r *http.Request) {
	form, ok := uiForm(w, r)
	if !ok {
		uiGo(w, r, "/", "INVALID FORM")
		return
	}
	if err := saveCustomProvider(form.Get("name"), form.Get("base"), form.Get("keyEnv")); err != nil {
		page := uiHomePage("PROVIDER SAVE FAILED: " + strings.ToUpper(err.Error()))
		page.AddOpen, page.AddName = true, form.Get("name")
		page.AddBase, page.AddKeyEnv = form.Get("base"), form.Get("keyEnv")
		if page.AddKeyEnv == "" {
			page.AddKeyEnv = "API_KEY" // placeholder default never gets lost
		}
		renderPage(w, page)
		return
	}
	uiGo(w, r, "/", "PROVIDER SAVED")
}

func uiProviderSheet(w http.ResponseWriter, r *http.Request) {
	page, ok := providerSheetPage(r)
	if !ok {
		uiGo(w, r, "/", "INVALID PROVIDER")
		return
	}
	renderPage(w, page)
}

func providerSheetPage(r *http.Request) (uiPage, bool) {
	name := r.URL.Query().Get("name")
	view, err := providerView()
	if err != nil || name == "" || isDemoName(name) || !providerExists(view, name) {
		return uiPage{}, false
	}
	all, err := loadModels()
	if err != nil {
		return uiPage{Title: "Provider", Msg: "STATE UNAVAILABLE", Name: name, SheetMode: true, MsgMs: uiMsgMs(r)}, true
	}
	var provider dashboardProvider
	for _, p := range view.Providers {
		if p.Name == name {
			provider = p
			break
		}
	}
	return uiPage{
		Title:       "Provider: " + name,
		Msg:         r.URL.Query().Get("msg"),
		MsgMs:       uiMsgMs(r),
		Name:        name,
		Models:      all[name],
		KeyCount:    keyCount(name),
		KeySlots:    keySlots(name),
		Base:        provider.Base,
		DefaultBase: provider.DefaultBase,
		DocsURL:     provider.DocsURL,
		CanReset:    provider.CanReset,
		Demo:        provider.Demo,
		KeySet:      provider.KeySet,
		SheetMode:   true,
	}, true
}

func uiProviderKey(w http.ResponseWriter, r *http.Request) {
	form, ok := uiForm(w, r)
	if !ok {
		uiGo(w, r, "/", "INVALID FORM")
		return
	}
	name := strings.TrimSpace(form.Get("name"))
	action := form.Get("action")
	target := sheetTarget(name)
	if name == "" || isDemoName(name) {
		uiGo(w, r, target, "INVALID PROVIDER")
		return
	}
	if view, err := providerView(); err != nil || !providerExists(view, name) {
		uiGo(w, r, target, "INVALID PROVIDER")
		return
	}
	switch action {
	case "add":
		switch err := addKey(name, form.Get("key")); {
		case errors.Is(err, errKeyDuplicate):
			uiGo(w, r, target, "KEY ALREADY SAVED")
			return
		case errors.Is(err, errKeyLimit):
			uiGo(w, r, target, "KEY LIMIT REACHED")
			return
		case err != nil:
			uiGo(w, r, target, "INVALID KEY")
			return
		}
		uiGo(w, r, target, "KEY ADDED")
	case "test":
		index, err := strconv.Atoi(form.Get("index"))
		if err != nil {
			uiGo(w, r, target, "INVALID KEY")
			return
		}
		uiGo(w, r, target, testProviderKey(name, index))
	case "remove":
		index, err := strconv.Atoi(form.Get("index"))
		if err != nil || removeKeyAt(name, index-1) != nil {
			uiGo(w, r, target, "INVALID KEY")
			return
		}
		uiGo(w, r, target, "KEY REMOVED")
	case "clear":
		if err := saveKey(name, ""); err != nil {
			uiGo(w, r, target, "STATE WRITE FAILED")
			return
		}
		uiGo(w, r, target, "KEYS CLEARED")
	default:
		uiGo(w, r, target, "INVALID ACTION")
	}
}

// uiProviderRemove drops a custom provider together with its keys and models.
// Baked slots keep their compiled default, so they cannot be removed.
func uiProviderRemove(w http.ResponseWriter, r *http.Request) {
	form, ok := uiForm(w, r)
	if !ok {
		uiGo(w, r, "/", "INVALID FORM")
		return
	}
	name := strings.TrimSpace(form.Get("name"))
	if name == "" || isDemoName(name) {
		uiGo(w, r, "/", "INVALID PROVIDER")
		return
	}
	if isBakedName(name) {
		uiGo(w, r, "/", "BUILT IN PROVIDER")
		return
	}
	all, err := loadModels()
	if err != nil {
		uiGo(w, r, "/", "STATE UNAVAILABLE")
		return
	}
	// A name with no registry line, no models and no keys is not a provider;
	// refuse instead of reporting a removal that did nothing.
	if view, err := providerView(); err != nil ||
		(!providerExists(view, name) && len(all[name]) == 0 && keyCount(name) == 0) {
		uiGo(w, r, "/", "INVALID PROVIDER")
		return
	}
	path, err := keyFilePath(name)
	if err != nil {
		uiGo(w, r, "/", "STATE UNAVAILABLE")
		return
	}
	if err := removeStateFile(path); err != nil {
		uiGo(w, r, "/", "KEY REMOVE FAILED")
		return
	}
	if _, has := all[name]; has {
		delete(all, name)
		if err := saveModels(all); err != nil {
			uiGo(w, r, "/", "STATE WRITE FAILED")
			return
		}
	}
	if err := pruneCombo(name, ""); err != nil {
		uiGo(w, r, "/", "STATE WRITE FAILED")
		return
	}
	if err := removeRegistryEntry(name); err != nil {
		uiGo(w, r, "/", "PROVIDER REMOVE FAILED")
		return
	}
	uiGo(w, r, "/", "PROVIDER REMOVED")
}

func uiProviderModel(w http.ResponseWriter, r *http.Request) {
	form, ok := uiForm(w, r)
	if !ok {
		uiGo(w, r, "/", "INVALID FORM")
		return
	}
	name := strings.TrimSpace(form.Get("name"))
	action := form.Get("action")
	model := strings.TrimSpace(form.Get("model"))
	target := sheetTarget(name)
	if name == "" || isDemoName(name) {
		uiGo(w, r, "/", "INVALID PROVIDER")
		return
	}
	// A form with a missing model field must say so, never silently no-op.
	if !validModel(model) {
		uiGo(w, r, target, "INVALID MODEL")
		return
	}
	if action != "add" && action != "remove" {
		uiGo(w, r, target, "INVALID ACTION")
		return
	}
	all, err := loadModels()
	if err != nil {
		uiGo(w, r, target, "STATE UNAVAILABLE")
		return
	}
	list := all[name]
	index := -1
	for i, m := range list {
		if m == model {
			index = i
			break
		}
	}
	switch {
	case action == "remove" && index < 0:
		uiGo(w, r, target, "MODEL NOT FOUND")
		return
	case action == "add" && index >= 0:
		uiGo(w, r, target, "MODEL EXISTS")
		return
	case action == "add" && len(list) >= maxModelsPerProvider:
		uiGo(w, r, target, "MODELS FULL")
		return
	}
	msg := "MODEL ADDED"
	if action == "remove" {
		list = append(list[:index], list[index+1:]...)
		msg = "MODEL REMOVED"
	} else {
		list = append(list, model)
	}
	all[name] = list
	if err := saveModels(all); err != nil {
		uiGo(w, r, target, "STATE WRITE FAILED")
		return
	}
	if action == "remove" {
		if err := pruneCombo(name, model); err != nil {
			uiGo(w, r, target, "STATE WRITE FAILED")
			return
		}
	}
	uiGo(w, r, target, msg)
}

func comboAddOptions(r *http.Request) ([]uiOption, string, error) {
	view, err := providerView()
	if err != nil {
		return nil, "", err
	}
	models, err := loadModels()
	if err != nil {
		return nil, "", err
	}
	steps, err := loadCombo()
	if err != nil {
		return nil, "", err
	}
	active := make(map[string]bool, len(steps))
	for _, s := range steps {
		active[s.Provider+"\x00"+s.Model] = true
	}
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	options := make([]uiOption, 0)
	for _, p := range view.Providers {
		for _, m := range models[p.Name] {
			if q != "" && !strings.Contains(strings.ToLower(p.Name+"/"+m), q) {
				continue
			}
			options = append(options, uiOption{
				Provider: p.Name,
				Model:    m,
				Active:   active[p.Name+"\x00"+m],
			})
		}
	}
	return options, r.URL.Query().Get("q"), nil
}

func uiComboAddForm(w http.ResponseWriter, r *http.Request) {
	options, search, err := comboAddOptions(r)
	if err != nil {
		renderPage(w, uiPage{Title: "Add Model", Msg: "STATE UNAVAILABLE"})
		return
	}
	renderPage(w, uiPage{
		Title:   "Add Model",
		Options: options,
		Search:  search,
	})
}

// uiComboAddSheet renders the add-model form inside the dashboard sheet so the
// home document never navigates. Its form POSTs to /ui/combo/add?sheet=1.
func uiComboAddSheet(w http.ResponseWriter, r *http.Request) {
	options, search, err := comboAddOptions(r)
	if err != nil {
		renderPage(w, uiPage{Title: "Add Model", Msg: "STATE UNAVAILABLE", ComboAddMode: true, MsgMs: uiMsgMs(r)})
		return
	}
	renderPage(w, uiPage{
		Title:        "Add Model",
		ComboAddMode: true,
		Options:      options,
		Search:       search,
		Msg:          r.URL.Query().Get("msg"),
		MsgMs:        uiMsgMs(r),
	})
}

func uiComboAdd(w http.ResponseWriter, r *http.Request) {
	target := "/"
	if r.URL.Query().Get("sheet") == "1" {
		target = "/ui/combo/add/sheet"
	}
	form, ok := uiForm(w, r)
	if !ok {
		uiGo(w, r, target, "INVALID FORM")
		return
	}
	provider, model := form.Get("provider"), strings.TrimSpace(form.Get("model"))
	view, err := providerView()
	models, err2 := loadModels()
	if err != nil || err2 != nil || isDemoName(provider) || !providerExists(view, provider) || !validModel(model) {
		uiGo(w, r, target, "INVALID FIELD")
		return
	}
	ok = false
	for _, m := range models[provider] {
		if m == model {
			ok = true
			break
		}
	}
	if !ok {
		uiGo(w, r, target, "INVALID FIELD")
		return
	}
	steps, err := loadCombo()
	if err != nil {
		uiGo(w, r, target, "STATE UNAVAILABLE")
		return
	}
	step := stateStep{Provider: provider, Model: model}
	for _, s := range steps {
		if s == step {
			uiGo(w, r, target, "MODEL ALREADY IN COMBO")
			return
		}
	}
	if len(steps) >= maxComboSteps {
		uiGo(w, r, target, "COMBO FULL")
		return
	}
	if err := saveCombo(append(steps, step)); err != nil {
		uiGo(w, r, target, "STATE WRITE FAILED")
		return
	}
	uiGo(w, r, target, "MODEL ADDED")
}

func uiComboMove(w http.ResponseWriter, r *http.Request) {
	form, ok := uiForm(w, r)
	if !ok {
		uiGo(w, r, "/", "INVALID FORM")
		return
	}
	i, err := strconv.Atoi(form.Get("index"))
	if err != nil || i < 0 {
		uiGo(w, r, "/", "INVALID FIELD")
		return
	}
	steps, err := loadCombo()
	if err != nil || i >= len(steps) {
		uiGo(w, r, "/", "INVALID FIELD")
		return
	}
	switch form.Get("action") {
	case "up":
		if i == 0 {
			uiGo(w, r, "/", "")
			return
		}
		steps[i-1], steps[i] = steps[i], steps[i-1]
	case "down":
		if i == len(steps)-1 {
			uiGo(w, r, "/", "")
			return
		}
		steps[i+1], steps[i] = steps[i], steps[i+1]
	case "remove":
		steps = append(steps[:i], steps[i+1:]...)
	default:
		uiGo(w, r, "/", "INVALID FIELD")
		return
	}
	if err := saveCombo(steps); err != nil {
		uiGo(w, r, "/", "STATE WRITE FAILED")
		return
	}
	uiGo(w, r, "/", "")
}

// uiComboSheet renders the inner combo form document. Its form POSTs to
// /ui/combo/run with target self, so only the iframe navigates.
func uiComboSheet(w http.ResponseWriter, r *http.Request) {
	steps, err := loadCombo()
	if err != nil {
		renderPage(w, uiPage{Title: "Combo Run", Msg: "STATE UNAVAILABLE", ComboSheetMode: true})
		return
	}
	renderPage(w, uiPage{
		Title:          "Combo Run",
		ComboSheetMode: true,
		Combo:          uiSteps(steps),
		Msg:            r.URL.Query().Get("msg"),
		MsgMs:          uiMsgMs(r),
	})
}

func uiComboRun(w http.ResponseWriter) {
	view, err := providerView()
	if err != nil {
		renderPage(w, uiPage{Title: "Combo Run", Msg: "STATE UNAVAILABLE", ComboResultMode: true})
		return
	}
	steps, err := loadCombo()
	if err != nil {
		renderPage(w, uiPage{Title: "Combo Run", Msg: "STATE UNAVAILABLE", ComboResultMode: true})
		return
	}
	out := runComboSteps(view, steps)
	rounds := strconv.Itoa(out.Rounds)
	winner := out.Winner
	if winner == "" {
		winner = "NO WINNER AFTER " + rounds + " ROUNDS"
	} else {
		winner = "WINNER ON ROUND " + rounds + ": " + winner
	}
	renderPage(w, uiPage{
		Title:           "Combo Run",
		Winner:          winner,
		Results:         out.Steps,
		ComboResultMode: true,
	})
}
