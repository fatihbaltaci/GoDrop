package wizard

import (
	"bytes"
	"image/png"
	"strings"
	"testing"
)

func TestSampleImageIsAPictureOfTheMark(t *testing.T) {
	t.Parallel()
	data := SampleImage()
	img, err := png.Decode(strings.NewReader(data))
	if err != nil {
		t.Fatalf("the sample must be a PNG a browser will render: %v", err)
	}
	if b := img.Bounds(); b.Dx() != sampleSize || b.Dy() != sampleSize {
		t.Errorf("bounds = %v, want %dx%d", b, sampleSize, sampleSize)
	}
	// Small enough to sit next to a configuration file without comment.
	if len(data) > 64*1024 {
		t.Errorf("the sample is %d bytes; it is meant to be a few kilobytes", len(data))
	}

	// The middle of the drop is the drop, and the top corner is not: a picture
	// of nothing at all would pass every check above.
	middle := img.At(sampleSize/2, sampleSize*3/4)
	corner := img.At(2, 2)
	if middle == corner {
		t.Error("the drop and the background came out the same colour")
	}
	r, g, b, _ := middle.RGBA()
	if !(b > r && b > g) {
		t.Errorf("the middle of the drop is %v, want the blue of the mark", middle)
	}
}

func TestSampleImageIsTheSameEveryTime(t *testing.T) {
	t.Parallel()
	// Uninstall recognises the picture by its bytes, so drawing it twice has
	// to produce the same file.
	if !bytes.Equal([]byte(SampleImage()), []byte(SampleImage())) {
		t.Error("the sample must be deterministic")
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
