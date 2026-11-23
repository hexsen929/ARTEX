package server

import (
	"reflect"
	"testing"
)

func TestOperatorSurfaceMappingAcceptsOnlyRequestedClasses(t *testing.T) {
	got := parseSurfaceMapping("```json\n{\"requested\":\" LOGIC \",\"foreign\":\"auth\",\"invalid\":\"invalid\"}\n```", []string{"requested", "invalid"})
	want := map[string]string{"requested": "logic"}
	if !reflect.DeepEqual(got, want) {
		t.Fatal(got)
	}
	if len(parseSurfaceMapping("not JSON", []string{"requested"})) != 0 {
		t.Fatal("invalid response accepted")
	}
}
