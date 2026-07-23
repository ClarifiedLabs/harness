package tools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"harness/internal/llm"
)

const viewImageOnePixelPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAADElEQVR4nGP4z8AAAAMBAQDJ/pLvAAAAAElFTkSuQmCC"

func writeViewImagePNG(t *testing.T) string {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString(viewImageOnePixelPNG)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "screen.png")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestViewImageDispatchReturnsSafeRichResult(t *testing.T) {
	path := writeViewImagePNG(t)
	r := &Registry{}
	r.Register(viewImage{})
	input, err := json.Marshal(map[string]any{"path": path, "detail": "high", "future_key": true})
	if err != nil {
		t.Fatal(err)
	}
	call := llm.ToolCall{ID: "image_1", Name: "view_image", Input: input}
	res := r.Dispatch(context.Background(), call)
	if res.IsError || len(res.Content) != 1 {
		t.Fatalf("Dispatch = %+v", res)
	}
	image := res.Content[0]
	if image.Kind != llm.BlockImage || image.ImageData != viewImageOnePixelPNG || image.ImageMediaType != "image/png" || image.ImageDetail != "high" {
		t.Fatalf("image = %+v", image)
	}
	if strings.Contains(res.Text, path) || strings.Contains(res.Text, image.ImageData) {
		t.Fatalf("unsafe receipt = %q", res.Text)
	}
	if !strings.Contains(res.Text, "screen.png") || !strings.Contains(res.Text, "1x1") {
		t.Fatalf("receipt = %q", res.Text)
	}
	if modality, ok := r.RequiredModality(call); !ok || modality != "image" {
		t.Fatalf("RequiredModality = %q, %v", modality, ok)
	}
	if paths, ok := r.ReadPaths(call); !ok || len(paths) != 1 || paths[0] != path {
		t.Fatalf("ReadPaths = %v, %v", paths, ok)
	}
}

func TestViewImageDefaultsDetailHigh(t *testing.T) {
	path := writeViewImagePNG(t)
	result, err := (viewImage{}).RunRich(context.Background(), json.RawMessage(`{"path":`+strconv.Quote(path)+`}`))
	if err != nil {
		t.Fatalf("RunRich: %v", err)
	}
	if len(result.Content) != 1 || result.Content[0].ImageDetail != "high" || !strings.Contains(result.Text, "detail=high") {
		t.Fatalf("result = %+v, want default detail high", result)
	}
}

func TestViewImageAcceptsRelativePathAndAllDetailValues(t *testing.T) {
	data, err := base64.StdEncoding.DecodeString(viewImageOnePixelPNG)
	if err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "relative.png"), data, 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	t.Chdir(dir)

	for _, detail := range []string{"auto", "low", "high", "original"} {
		result, err := (viewImage{}).RunRich(context.Background(), json.RawMessage(`{"path":"relative.png","detail":"`+detail+`"}`))
		if err != nil {
			t.Fatalf("RunRich detail %q: %v", detail, err)
		}
		if len(result.Content) != 1 || result.Content[0].ImageDetail != detail {
			t.Fatalf("detail %q result = %+v", detail, result)
		}
	}
}

func TestViewImageDispatchErrorsStayTextOnly(t *testing.T) {
	r := &Registry{}
	r.Register(viewImage{})
	for _, input := range []json.RawMessage{
		json.RawMessage(`{"detail":"auto"}`),
		json.RawMessage(`{"path":"missing.png","detail":"zoom"}`),
		json.RawMessage(`{"path":"missing.png"}`),
		json.RawMessage(`{"path":"."}`),
		json.RawMessage(`{"path":`),
	} {
		res := r.Dispatch(context.Background(), llm.ToolCall{ID: "image_1", Name: "view_image", Input: input})
		if !res.IsError || len(res.Content) != 0 {
			t.Fatalf("Dispatch(%s) = %+v, want text-only error", input, res)
		}
	}
}

func TestViewImageCanceledContextReturnsTextOnlyError(t *testing.T) {
	path := writeViewImagePNG(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := &Registry{}
	r.Register(viewImage{})
	input := json.RawMessage(`{"path":` + strconv.Quote(path) + `}`)
	res := r.Dispatch(ctx, llm.ToolCall{ID: "image_1", Name: "view_image", Input: input})
	if !res.IsError || len(res.Content) != 0 || !strings.Contains(res.Text, context.Canceled.Error()) {
		t.Fatalf("canceled Dispatch = %+v, want text-only cancellation error", res)
	}
}

func TestViewImageIsReadOnlyAndRegisteredByDefault(t *testing.T) {
	r := Default()
	call := llm.ToolCall{Name: "view_image", Input: json.RawMessage(`{"path":"screen.png"}`)}
	if !r.CallReadOnly(call) {
		t.Fatal("view_image is not read-only")
	}
	if !containsName(r.Names(), "view_image") {
		t.Fatalf("default names = %v", r.Names())
	}
}

func containsName(names []string, want string) bool {
	for _, name := range names {
		if name == want {
			return true
		}
	}
	return false
}
