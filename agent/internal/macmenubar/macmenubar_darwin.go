//go:build darwin

// Package macmenubar shows a small macOS menu bar icon (NSStatusItem) -
// the same "always visible, click to check on it" role the Windows tray
// icon plays. It runs as its own short-lived helper process
// (`backup-agent --menubar`), launched by the LaunchDaemon into the
// console user's session (see macdaemon.LaunchInConsoleSession) since a
// root daemon has no window server session of its own to draw into. Like
// the Windows tray, it talks to the daemon purely over the local control
// API on 127.0.0.1 (see cmd/backup-agent's startTrayControlAPI) - if the
// daemon is unreachable or this process misbehaves, the actual backup
// service is entirely unaffected, since it's a separate OS process.
//
// Driven straight through the Objective-C runtime via purego
// (github.com/ebitengine/purego), no CGO: the same "talk to the OS
// directly" approach the Windows tray takes with raw Win32 syscalls, just
// aimed at libobjc/AppKit instead of user32/shell32. This keeps the agent
// cross-compilable for macOS from a plain Linux Go toolchain, with no
// Xcode or macOS SDK involved in the build.
package macmenubar

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"runtime"
	"time"

	"github.com/ebitengine/purego"

	"backup-agent/internal/osui"
)

const (
	// NSApplicationActivationPolicyAccessory: no Dock icon, no app switcher
	// entry, no menu bar (App menu) of its own - just the status item.
	activationPolicyAccessory  = 1
	nsVariableStatusItemLength = -1.0

	tagBackupNow  = 1
	tagReschedule = 2
	tagOpenPanel  = 3
)

// objc bundles every Objective-C runtime entry point this package calls,
// each bound once at startup. objc_msgSend is registered under several Go
// function types (msgSend*) rather than one: the real C signature of
// objc_msgSend depends entirely on the message being sent (how many
// arguments, whether an argument is a float, what the return type is),
// and purego needs the exact Go type at each call site to place arguments
// in the right registers. All of them resolve to the very same underlying
// function pointer - this is the standard, documented purego pattern for
// driving Objective-C without cgo.
type objc struct {
	getClass          func(name string) uintptr
	selRegisterName   func(name string) uintptr
	allocateClassPair func(superclass uintptr, name string, extraBytes uintptr) uintptr
	registerClassPair func(cls uintptr)
	classAddMethod    func(cls, sel, imp uintptr, types string) bool

	msgSend         func(id, sel uintptr) uintptr
	msgSendVoidPtr  func(id, sel, arg uintptr)
	msgSendBoolInt  func(id, sel uintptr, arg int) bool
	msgSendFloat    func(id, sel uintptr, arg float64) uintptr
	msgSendStr      func(id, sel uintptr, arg string) uintptr
	msgSendVoidInt  func(id, sel uintptr, arg int)
	msgSendGetInt   func(id, sel uintptr) int
	msgSendInitItem func(id, sel, title, action, key uintptr) uintptr
}

func loadObjC() (*objc, error) {
	libobjc, err := purego.Dlopen("/usr/lib/libobjc.A.dylib", purego.RTLD_NOW|purego.RTLD_GLOBAL)
	if err != nil {
		return nil, fmt.Errorf("libobjc: %w", err)
	}
	// AppKit pulls in Foundation (NSString, ...) itself; loading it
	// explicitly here just makes the dependency visible rather than
	// relying on that transitive link.
	if _, err := purego.Dlopen("/System/Library/Frameworks/Foundation.framework/Foundation", purego.RTLD_NOW|purego.RTLD_GLOBAL); err != nil {
		return nil, fmt.Errorf("Foundation: %w", err)
	}
	if _, err := purego.Dlopen("/System/Library/Frameworks/AppKit.framework/AppKit", purego.RTLD_NOW|purego.RTLD_GLOBAL); err != nil {
		return nil, fmt.Errorf("AppKit: %w", err)
	}

	o := &objc{}
	purego.RegisterLibFunc(&o.getClass, libobjc, "objc_getClass")
	purego.RegisterLibFunc(&o.selRegisterName, libobjc, "sel_registerName")
	purego.RegisterLibFunc(&o.allocateClassPair, libobjc, "objc_allocateClassPair")
	purego.RegisterLibFunc(&o.registerClassPair, libobjc, "objc_registerClassPair")
	purego.RegisterLibFunc(&o.classAddMethod, libobjc, "class_addMethod")

	msgSendAddr, err := purego.Dlsym(libobjc, "objc_msgSend")
	if err != nil {
		return nil, fmt.Errorf("objc_msgSend: %w", err)
	}
	purego.RegisterFunc(&o.msgSend, msgSendAddr)
	purego.RegisterFunc(&o.msgSendVoidPtr, msgSendAddr)
	purego.RegisterFunc(&o.msgSendBoolInt, msgSendAddr)
	purego.RegisterFunc(&o.msgSendFloat, msgSendAddr)
	purego.RegisterFunc(&o.msgSendStr, msgSendAddr)
	purego.RegisterFunc(&o.msgSendVoidInt, msgSendAddr)
	purego.RegisterFunc(&o.msgSendGetInt, msgSendAddr)
	purego.RegisterFunc(&o.msgSendInitItem, msgSendAddr)
	return o, nil
}

func (o *objc) nsString(s string) uintptr {
	return o.msgSendStr(o.getClass("NSString"), o.selRegisterName("stringWithUTF8String:"), s)
}

// menuItem creates an NSMenuItem. action == 0 makes it a plain,
// non-interactive line (AppKit greys out and disables any item with no
// action automatically) - used for the informational "last backup" line.
func (o *objc) menuItem(title string, action uintptr) uintptr {
	item := o.msgSend(o.getClass("NSMenuItem"), o.selRegisterName("alloc"))
	sel := o.selRegisterName("initWithTitle:action:keyEquivalent:")
	return o.msgSendInitItem(item, sel, o.nsString(title), action, o.nsString(""))
}

func (o *objc) setTitle(item uintptr, title string) {
	o.msgSendVoidPtr(item, o.selRegisterName("setTitle:"), o.nsString(title))
}

func (o *objc) addItem(menu, item uintptr) {
	o.msgSendVoidPtr(menu, o.selRegisterName("addItem:"), item)
}

func (o *objc) setTag(item uintptr, tag int) {
	o.msgSendVoidInt(item, o.selRegisterName("setTag:"), tag)
}

func (o *objc) tag(item uintptr) int {
	return o.msgSendGetInt(item, o.selRegisterName("tag"))
}

type trayState struct {
	DeviceName       string `json:"device_name"`
	LastBackupAt     string `json:"last_backup_at"`
	LastBackupStatus string `json:"last_backup_status"`
	Connected        bool   `json:"connected"`
}

func fetchState(base string, client *http.Client) (*trayState, error) {
	resp, err := client.Get(base + "/tray/state")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var st trayState
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return nil, err
	}
	return &st, nil
}

func summarize(st *trayState) string {
	status := "jamais"
	if st.LastBackupAt != "" {
		if t, err := time.Parse(time.RFC3339, st.LastBackupAt); err == nil {
			status = t.Local().Format("02/01 15:04")
			if st.LastBackupStatus == "failed" {
				status += " (échec)"
			}
		}
	}
	conn := "hors ligne"
	if st.Connected {
		conn = "en ligne"
	}
	return "Dernière sauvegarde : " + status + " · " + conn
}

// Run sets up the status item and blocks until the process is killed
// (normal for a helper meant to stay resident for the whole session).
func Run(controlBase string) error {
	// Cocoa requires its calls to happen on the process's real main
	// thread. LockOSThread here pins the calling goroutine (main(), the
	// very first thing it does for this subcommand) to whatever OS thread
	// it is currently on - which, this early, is that main thread.
	runtime.LockOSThread()

	o, err := loadObjC()
	if err != nil {
		return err
	}

	app := o.msgSend(o.getClass("NSApplication"), o.selRegisterName("sharedApplication"))
	o.msgSendBoolInt(app, o.selRegisterName("setActivationPolicy:"), activationPolicyAccessory)

	statusBar := o.msgSend(o.getClass("NSStatusBar"), o.selRegisterName("systemStatusBar"))
	statusItem := o.msgSendFloat(statusBar, o.selRegisterName("statusItemWithLength:"), nsVariableStatusItemLength)
	button := o.msgSend(statusItem, o.selRegisterName("button"))
	// A plain glyph rather than a bundled image: no icon asset to embed,
	// and it renders correctly at every Retina scale for free.
	o.setTitle(button, "💾")
	o.msgSendVoidPtr(button, o.selRegisterName("setToolTip:"), o.nsString("Backup Agent"))

	// A single dynamically-registered Objective-C class backs every
	// clickable item; which one was clicked is read back from the item's
	// tag rather than giving each item its own method, so there is only
	// one selector/IMP pair to get right.
	targetCls := o.allocateClassPair(o.getClass("NSObject"), "BackupAgentMenuTarget", 0)
	selInvoke := o.selRegisterName("invoke:")
	httpClient := &http.Client{Timeout: 5 * time.Second}
	imp := purego.NewCallback(func(self, cmd, sender uintptr) uintptr {
		switch o.tag(sender) {
		case tagBackupNow:
			go func() {
				req, _ := http.NewRequest(http.MethodPost, controlBase+"/tray/backup-now", nil)
				if req != nil {
					_, _ = httpClient.Do(req)
				}
			}()
		case tagReschedule:
			_ = osui.OpenBrowser(controlBase + "/tray/reschedule-page")
		case tagOpenPanel:
			_ = osui.OpenBrowser(controlBase + "/tray/open-panel")
		}
		return 0
	})
	if !o.classAddMethod(targetCls, selInvoke, imp, "v@:@") {
		return fmt.Errorf("class_addMethod: échec de l'enregistrement de l'action du menu")
	}
	o.registerClassPair(targetCls)
	target := o.msgSend(o.msgSend(targetCls, o.selRegisterName("alloc")), o.selRegisterName("init"))

	menu := o.msgSend(o.msgSend(o.getClass("NSMenu"), o.selRegisterName("alloc")), o.selRegisterName("init"))

	lastBackupItem := o.menuItem("Dernière sauvegarde : chargement…", 0)
	o.addItem(menu, lastBackupItem)
	o.addItem(menu, o.msgSend(o.getClass("NSMenuItem"), o.selRegisterName("separatorItem")))

	addAction := func(title string, tag int) {
		item := o.menuItem(title, selInvoke)
		o.msgSendVoidPtr(item, o.selRegisterName("setTarget:"), target)
		o.setTag(item, tag)
		o.addItem(menu, item)
	}
	addAction("Sauvegarder maintenant", tagBackupNow)
	addAction("Reprogrammer la prochaine sauvegarde…", tagReschedule)
	o.addItem(menu, o.msgSend(o.getClass("NSMenuItem"), o.selRegisterName("separatorItem")))
	addAction("Ouvrir le panneau", tagOpenPanel)

	o.msgSendVoidPtr(statusItem, o.selRegisterName("setMenu:"), menu)

	// Keeps the "last backup" line current. Runs on its own goroutine, off
	// the locked main thread that's about to block in [NSApp run] below -
	// updating a menu item's title is a plain property set with no
	// synchronous drawing of its own (AppKit only reads it back lazily,
	// when the menu is actually about to be shown), which in practice
	// every minimal Cocoa menu bar utility relies on to avoid needing a
	// dispatch-to-main-thread mechanism for something this small.
	go func() {
		const refreshInterval = 30 * time.Second
		const unreachableLimit = 10 // ~5 minutes of no reply at all
		consecutiveFailures := 0
		for {
			if st, err := fetchState(controlBase, httpClient); err == nil {
				consecutiveFailures = 0
				o.setTitle(lastBackupItem, summarize(st))
			} else {
				consecutiveFailures++
				o.setTitle(lastBackupItem, "Backup Agent — service injoignable")
				if consecutiveFailures >= unreachableLimit {
					log.Print("barre de menu : service injoignable depuis longtemps, arrêt")
					os.Exit(0)
				}
			}
			time.Sleep(refreshInterval)
		}
	}()

	log.Print("icône de la barre de menu prête")
	o.msgSend(app, o.selRegisterName("run")) // [NSApp run] - blocks for the process's whole life
	return nil
}
