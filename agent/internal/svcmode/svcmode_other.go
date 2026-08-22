//go:build !windows

package svcmode

import (
	"context"
	"errors"
)

var errUnsupported = errors.New("svcmode: disponible uniquement sur Windows")

func IsWindowsService() bool                  { return false }
func Run(run func(ctx context.Context)) error { return errUnsupported }
func Install(exePath string) error            { return errUnsupported }
func Uninstall() error                        { return errUnsupported }
