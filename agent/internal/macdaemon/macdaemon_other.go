//go:build !darwin

package macdaemon

import "errors"

var errUnsupported = errors.New("macdaemon: disponible uniquement sur macOS")

func IsRoot() bool                                               { return false }
func Install(exePath string) error                               { return errUnsupported }
func Uninstall() error                                           { return errUnsupported }
func ConsoleUser() (username string, uid int, err error)         { return "", 0, errUnsupported }
func ConsoleUserHomeDir() (string, error)                        { return "", errUnsupported }
func LaunchInConsoleSession(exePath string, args []string) error { return errUnsupported }
