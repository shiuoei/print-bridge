//go:build !windows

package convert

import (
	"context"
	"errors"
)

func windowsOfficePDF(context.Context, string, string, string) (string, error) {
	return "", errors.New("not Windows")
}
