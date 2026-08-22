//go:build !windows

package winsession

import "errors"

var errUnsupported = errors.New("winsession: disponible uniquement sur Windows")

func LaunchInConsoleSession(exePath string, args []string) error { return errUnsupported }
func ConsoleUserHomeDir() (string, error)                        { return "", errUnsupported }
func ConsoleUserSID() (string, error)                            { return "", errUnsupported }
