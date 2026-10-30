package model

import (
	"errors"
	"strings"
	"testing"
)

func TestNewIDPrefixes(t *testing.T) {
	ws := NewWorkspaceID()
	if !strings.HasPrefix(ws, WorkspaceIDPrefix) || len(ws) != len(WorkspaceIDPrefix)+26 {
		t.Fatalf("bad workspace id %q", ws)
	}
	run := NewRunID()
	if !strings.HasPrefix(run, RunIDPrefix) || len(run) != len(RunIDPrefix)+26 {
		t.Fatalf("bad run id %q", run)
	}
	if ws == run {
		t.Fatal("ids should be unique")
	}
}

func TestErrorFormatting(t *testing.T) {
	e := New(422, CodeUnprocessable, "bad path")
	if e.Error() != "unprocessable: bad path" {
		t.Fatalf("err = %q", e.Error())
	}
	var nilErr *Error
	if nilErr.Error() != "" {
		t.Fatal("nil error should be empty")
	}
}

func TestAsError(t *testing.T) {
	if got := AsError(nil); got != nil {
		t.Fatal("nil input")
	}
	wrapped := New(403, CodeForbidden, "nope")
	if got := AsError(wrapped); got != wrapped {
		t.Fatal("should return original")
	}
	generic := AsError(errors.New("boom"))
	if generic.Status != 500 || generic.Code != CodeInternal {
		t.Fatalf("got %+v", generic)
	}
}
