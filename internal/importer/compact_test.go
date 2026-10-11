package importer

import (
	"strings"
	"testing"

	"github.com/vipinmohan22/layalogs/internal/model"
)

func TestCompactMarksCutEvents(t *testing.T) {
	if ev := compact(model.Event{Message: "short"}); ev.Compacted {
		t.Error("an event kept whole is not compacted")
	}
	if ev := compact(model.Event{Message: strings.Repeat("x", retainedMsgBytes+1)}); !ev.Compacted {
		t.Error("a cut message must be marked")
	}
	if ev := compact(model.Event{Message: "m", Fields: map[string]any{"big": strings.Repeat("y", retainedFieldsBytes+1)}}); !ev.Compacted {
		t.Error("dropped fields must be marked")
	}
}
