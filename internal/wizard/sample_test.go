package wizard

import (
	"crypto/sha256"
	"encoding/hex"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSampleImageIsAPictureOfTheLogo(t *testing.T) {
	t.Parallel()
	data := SampleImage()
	img, err := png.Decode(strings.NewReader(data))
	if err != nil {
		t.Fatalf("the sample must be a PNG a browser will render: %v", err)
	}
	// The lockup is wider than it is tall; a square would mean the wrong file
	// was rendered, or that a renderer padded it.
	if b := img.Bounds(); b.Dx() <= b.Dy() {
		t.Errorf("bounds = %v, want the shape of the lockup", b)
	}
	// Small enough to sit next to a configuration file without comment.
	if len(data) > 256*1024 {
		t.Errorf("the sample is %d bytes; it is meant to be a picture, not a payload", len(data))
	}
}

func TestSampleImageMatchesTheLogoItWasRenderedFrom(t *testing.T) {
	t.Parallel()
	// The picture is generated, and a generated file that no longer matches
	// its source is the one failure a committed asset can have. Comparing the
	// stamp catches it without needing a renderer on this machine.
	source, err := os.ReadFile(filepath.Join("..", "..", "site", "logo-lockup.svg"))
	if err != nil {
		t.Fatal(err)
	}
	stamped, err := os.ReadFile(filepath.Join("assets", "sample.source"))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(source)
	if got, want := strings.TrimSpace(string(stamped)), hex.EncodeToString(sum[:]); got != want {
		t.Errorf("the logo changed but the picture was not re-rendered.\nRun: make docs\n got %s\nwant %s", got, want)
	}
}

func TestFilesIncludeThePictureTheExampleUploads(t *testing.T) {
	t.Parallel()
	var found *GeneratedFile
	for _, f := range Files(Defaults(), "") {
		if f.Name == SampleName {
			found = &f
		}
	}
	if found == nil {
		t.Fatalf("setup should write %s", SampleName)
	}
	if !strings.HasPrefix(found.Body, "\x89PNG") {
		t.Error("the sample should be written as a PNG")
	}
	if found.Perm != 0o644 {
		t.Errorf("mode = %#o; the picture is not a secret", found.Perm)
	}
}

func TestTheFirstExampleUploadsTheSample(t *testing.T) {
	t.Parallel()
	a := Defaults()
	a.Token = "gd_abc"
	upload := CurlExamplesFor("linux", a, "/home/you/.godrop/"+SampleName)[0]
	if !strings.Contains(upload, `-F "file=@/home/you/.godrop/`+SampleName+`"`) {
		t.Errorf("the example should upload the picture setup wrote:\n%s", upload)
	}
	// Without one, the documentation's photo.jpg is still the better example.
	if !strings.Contains(CurlExamplesFor("linux", a, "")[0], "photo.jpg") {
		t.Error("with no sample the example should fall back to photo.jpg")
	}
}
