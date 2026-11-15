package agent

import (
	"bytes"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCaptureScreenCustomCommand(t *testing.T) {
	var b bytes.Buffer
	png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 2, 2)))
	f := filepath.Join(t.TempDir(), "shot.png")
	os.WriteFile(f, b.Bytes(), 0o600)
	img, how, err := captureScreen(t.Context(), []string{"cat", f})
	if err != nil || how != "cat" || !bytes.Equal(img, b.Bytes()) {
		t.Fatalf("capture: %v %q", err, how)
	}
	// A command that doesn't produce an image is an error, not an upload.
	if _, _, err := captureScreen(t.Context(), []string{"echo", "hello"}); err == nil || !strings.Contains(err.Error(), "PNG or JPEG") {
		t.Fatalf("non-image: %v", err)
	}
}
