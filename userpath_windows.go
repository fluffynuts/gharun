package main

import (
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// addToUserPath appends dir to the user's PATH (HKCU\Environment) and tells
// running programs the environment changed; terminals that are already open
// still need restarting to see it. added is false when the user PATH already
// holds dir.
func addToUserPath(dir string) (added bool, err error) {
	key, err := registry.OpenKey(registry.CURRENT_USER, "Environment", registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return false, err
	}
	defer key.Close()

	current, kind, err := key.GetStringValue("Path")
	if err == registry.ErrNotExist {
		current, kind = "", registry.EXPAND_SZ
	} else if err != nil {
		return false, err
	}
	for _, p := range filepath.SplitList(current) {
		if expanded, err := registry.ExpandString(p); err == nil {
			p = expanded
		}
		if p != "" && strings.EqualFold(filepath.Clean(p), filepath.Clean(dir)) {
			return false, nil
		}
	}

	updated := dir
	if current = strings.TrimRight(current, ";"); current != "" {
		updated = current + ";" + dir
	}
	if kind == registry.EXPAND_SZ {
		err = key.SetExpandStringValue("Path", updated)
	} else {
		err = key.SetStringValue("Path", updated)
	}
	if err != nil {
		return false, err
	}
	broadcastEnvironmentChange()
	return true, nil
}

// broadcastEnvironmentChange sends WM_SETTINGCHANGE so Explorer (and so new
// terminals it starts) picks up the new PATH. Best effort.
func broadcastEnvironmentChange() {
	const (
		hwndBroadcast   = 0xffff
		wmSettingChange = 0x001a
		smtoAbortIfHung = 0x0002
	)
	env, err := windows.UTF16PtrFromString("Environment")
	if err != nil {
		return
	}
	var result uintptr
	windows.NewLazySystemDLL("user32.dll").NewProc("SendMessageTimeoutW").Call(
		hwndBroadcast, wmSettingChange, 0, uintptr(unsafe.Pointer(env)),
		smtoAbortIfHung, 5000, uintptr(unsafe.Pointer(&result)))
}
