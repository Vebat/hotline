package hotline_test

import (
	"testing"

	"github.com/Vebat/hotline"
	"github.com/Vebat/hotline/storetest"
)

func TestMemory(t *testing.T) {
	storetest.Run(t, func() hotline.Store { return &hotline.Memory{} })
}
