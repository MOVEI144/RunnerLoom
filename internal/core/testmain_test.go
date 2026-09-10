package core

import (
	"os"
	"testing"

	"github.com/MOVEI144/RunnerLoom/internal/testsupport"
)

func TestMain(m *testing.M) {
	os.Exit(testsupport.Run(m))
}
