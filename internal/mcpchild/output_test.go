package mcpchild

import (
	"context"
	"io"
	"testing"
	"time"
)

func TestChildFinalOutputSurvivesExit(t *testing.T) {
	child, err := Spawn("/bin/sh", []string{"-c", "printf 'final response'"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer child.Close(context.Background())
	select {
	case <-child.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("child did not exit")
	}
	got, err := io.ReadAll(child.Conn())
	if err != nil || string(got) != "final response" {
		t.Fatalf("buffered output after child exit = %q, %v", got, err)
	}
}
