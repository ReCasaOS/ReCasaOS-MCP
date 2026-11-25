package catalogue

import (
	"embed"
	"fmt"
)

//go:embed v1/*.yaml
var embedded embed.FS

// Embedded returns the descriptions written by hand for routes the box does not describe
// itself, by file name (e.g. "v1/system.yaml").
func Embedded() map[string][]byte {
	files, err := embedded.ReadDir("v1")
	if err != nil {
		panic(fmt.Sprintf("catalogue/v1: %v", err)) // embedded: a test catches it
	}
	out := map[string][]byte{}
	for _, f := range files {
		raw, err := embedded.ReadFile("v1/" + f.Name())
		if err != nil {
			panic(fmt.Sprintf("catalogue/v1/%s: %v", f.Name(), err))
		}
		out["v1/"+f.Name()] = raw
	}
	return out
}
