// Package catalogue holds the classification of the box's API actions: how
// dangerous each one is. An action that is not classified is not offered.
package catalogue

import (
	_ "embed"
	"fmt"
	"maps"
	"sync"

	yaml "github.com/oasdiff/yaml3"
)

// Class says how dangerous an action is.
type Class string

const (
	Read   Class = "read"
	Secret Class = "secret"
	Change Class = "change"
	Danger Class = "danger"
	Never  Class = "never"
)

// Valid reports whether c is one of the five classes.
func (c Class) Valid() bool {
	switch c {
	case Read, Secret, Change, Danger, Never:
		return true
	}
	return false
}

//go:embed classes.yaml
var classesFile []byte

var classes = sync.OnceValue(func() map[string]Class {
	m := map[string]Class{}
	if err := yaml.Unmarshal(classesFile, &m); err != nil {
		panic(fmt.Sprintf("catalogue/classes.yaml: %v", err)) // embedded: a test catches it
	}
	return m
})

// Classes returns the action classes, keyed by "METHOD /full/path/{param}".
// The caller gets its own copy; values are not checked here (see Valid).
func Classes() map[string]Class { return maps.Clone(classes()) }

// Sources returns the paths, on the box, of the descriptions to read.
// The message bus and the module's own description are left out on purpose.
func Sources() []string {
	return []string{
		"/doc/v2/casaos/openapi.yaml",
		"/doc/v2/app_management/openapi.yaml",
		"/v1doc/v1/app_management/openapi_v1.yaml",
		"/doc/v2/local_storage/openapi.yaml",
		"/doc/v2/users/openapi.yaml",
		"/doc/v2/virt_management/openapi.yaml",
		"/doc/v2/vuln_scan/openapi.yaml",
	}
}
