package loadgen

// Exported for hashcore_test.go, an external test package (loadgen_test)
// so that its hashcore import is confined to that test. This file is
// compiled only into this package's tests.

// Form is one form of a negative class.
type Form struct {
	Name  string
	Build func() Request
}

// Forms returns every form of class c.
func Forms(c NegativeClass) []Form {
	var fs []Form
	for _, f := range negativeForms[c] {
		fs = append(fs, Form{f.name, f.build})
	}
	return fs
}

var RandomExactPHC = randomExactPHC

// For enumerating the malformed-hash forms' variable parts.
var (
	ExactPHC        = exactPHC
	B64             = b64
	LastCut         = lastCut
	InsertNewlineAt = insertNewlineAt
	UnusedBitValues = unusedBitValues
	SetUnusedBitsTo = setUnusedBitsTo
)

const (
	SaltLen = saltLen
	KeyLen  = keyLen
)
