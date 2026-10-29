package server

import (
	"errors"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/llm"
)

func TestMergeAutoTitleExplicitSelectionNeverFallsBack(t *testing.T) {
	const selectedID = int64(41)
	selected := selectedID
	activeCalled := false
	providerCalls := 0
	provider, err := resolveAutoTitleProvider(&selected, func(id int64) (llm.Provider, bool) {
		providerCalls++
		if id != selectedID {
			t.Fatalf("selected profile changed to %d", id)
		}
		return nil, false
	}, func() (*db.LLMProfile, error) {
		activeCalled = true
		return &db.LLMProfile{ID: 99}, nil
	})
	if err == nil || provider != nil || activeCalled || providerCalls != 1 {
		t.Fatalf("invalid explicit selection silently rerouted: active=%v calls=%d error=%v", activeCalled, providerCalls, err)
	}
}

func TestMergeAutoTitleUnselectedMayUseActiveProfile(t *testing.T) {
	want := agent.ErrorProvider(errors.New("fixture provider, never called"))
	provider, err := resolveAutoTitleProvider(nil, func(id int64) (llm.Provider, bool) {
		if id != 99 {
			t.Fatalf("unexpected active profile %d", id)
		}
		return want, true
	}, func() (*db.LLMProfile, error) { return &db.LLMProfile{ID: 99}, nil })
	if err != nil || provider != want {
		t.Fatalf("unselected global fallback failed: %v", err)
	}
	for _, activeError := range []error{nil, errors.New("fixture lookup error")} {
		called := false
		provider, err = resolveAutoTitleProvider(nil, func(int64) (llm.Provider, bool) {
			called = true
			return want, true
		}, func() (*db.LLMProfile, error) { return nil, activeError })
		if err == nil || provider != nil || called {
			t.Fatalf("missing/failed active selection reached a provider: %v", err)
		}
	}
}

func TestMergeReporterTriggerNormalizationKeepsEventData(t *testing.T) {
	const marker = "【本次由工具调用触发】"
	const event = marker + " fixture finding_id=31"
	got := normalizeReporterTriggerMessage("retired update_finding_reproduction prompt\n" + event)
	if got != reporterToolCallMessage+event || strings.Contains(got, "retired update_finding_reproduction") {
		t.Fatal("normalization either kept a retired contract or lost the original event")
	}
}
