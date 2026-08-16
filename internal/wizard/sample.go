package wizard

import _ "embed"

// SampleName is the picture setup leaves next to the configuration, so that
// the first command it prints is one that works: an example can only be
// pasted if the file it names exists, and this is that file.
const SampleName = "sample.png"

// The logo, rendered from site/logo-lockup.svg by scripts/sample.py. It is
// carried rather than drawn because Go has no SVG renderer, and rendered
// ahead of time rather than fetched because setup must work on a machine with
// nothing else on it.
//
//go:embed assets/sample.png
var sampleImage []byte

// SampleImage is that picture, as the bytes to write.
func SampleImage() string { return string(sampleImage) }
