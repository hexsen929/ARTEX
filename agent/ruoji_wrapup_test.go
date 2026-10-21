package agent

import (
	"reflect"
	"testing"
)

func TestRuojiWrapupHasNoIndependentSecondLimit(t *testing.T) {
	config := wrapupSettlement("worker", []string{"Bash"})
	if config.MaxTurns != WrapupTurnsDefault("worker") {
		t.Fatalf("wrap-up turns=%d", config.MaxTurns)
	}
	if field := reflect.ValueOf(*config).FieldByName("MaxDuration"); field.IsValid() && field.Int() != 0 {
		t.Fatal("an independent wrap-up deadline was reintroduced")
	}
	found := false
	for _, name := range config.DisabledTools {
		if name == "Bash" {
			found = true
		}
	}
	if !found {
		t.Fatal("wrap-up execution restriction changed")
	}
}
