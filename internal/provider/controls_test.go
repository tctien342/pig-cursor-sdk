package provider

import (
	"reflect"
	"testing"
)

func TestControlsEmbeddedThinking(t *testing.T) {
	items, err := FallbackCatalog()
	if err != nil {
		t.Fatal(err)
	}
	catalog := BuildCatalog(items, nil)
	for _, tc := range []struct{ model, level, param, value string }{
		{"claude-opus-5", "off", "thinking", "false"},
		{"claude-opus-5", "high", "effort", "high"},
		{"gpt-5.5", "xhigh", "reasoning", "extra-high"},
		{"deepseek-v41-flash-1017", "off", "reasoning_effort", "none"},
		{"claude-opus-4-5", "off", "thinking", "false"},
		{"claude-opus-4-5", "high", "thinking", "true"},
	} {
		t.Run(tc.model+"/"+tc.level, func(t *testing.T) {
			entry := catalog.Lookup(tc.model)
			if entry == nil {
				t.Fatal("missing native alias")
			}
			before := append([]Param(nil), entry.Params...)
			got := resolvedParams(entry, ModelOverride{Thinking: tc.level})
			if value := paramValue(got, tc.param); value != tc.value {
				t.Fatalf("%s=%q, want %q; params=%v", tc.param, value, tc.value, got)
			}
			if tc.model == "claude-opus-5" && tc.level == "off" && paramValue(got, "effort") != paramValue(before, "effort") {
				t.Fatal("off changed effort")
			}
			if !reflect.DeepEqual(before, entry.Params) {
				t.Fatal("defaults mutated")
			}
			if !reflect.DeepEqual(resolvedParams(entry, ModelOverride{Thinking: "unsupported"}), before) {
				t.Fatal("unsupported selection changed defaults")
			}
			got[0].Value = "mutation"
			if !reflect.DeepEqual(before, entry.Params) {
				t.Fatal("result aliases defaults")
			}
		})
	}
}

func TestControlsBooleanAndEffort(t *testing.T) {
	items, err := ParseModelItems([]byte(`[{"id":"both","parameters":[{"id":"thinking","values":[{"value":"false"},{"value":"true"}]},{"id":"effort","values":[{"value":"low"},{"value":"high"}]}],"variants":[{"isDefault":true,"params":[{"id":"thinking","value":"false"},{"id":"effort","value":"low"}]}]},{"id":"off","parameters":[{"id":"reasoning_effort","values":[{"value":"off"},{"value":"high"}]}]}]`))
	if err != nil {
		t.Fatal(err)
	}
	catalog := BuildCatalog(items, nil)
	entry := catalog.Lookup("both")
	got := resolvedParams(entry, ModelOverride{Thinking: "high"})
	if paramValue(got, "thinking") != "true" || paramValue(got, "effort") != "high" {
		t.Fatal(got)
	}
	off := resolvedParams(entry, ModelOverride{Thinking: "off"})
	if paramValue(off, "thinking") != "false" || paramValue(off, "effort") != "low" {
		t.Fatal(off)
	}
	if canonicalParamSignature(off) == canonicalParamSignature(got) {
		t.Fatal("different controls share signature")
	}
	if paramValue(resolvedParams(catalog.Lookup("off"), ModelOverride{Thinking: "off"}), "reasoning_effort") != "off" {
		t.Fatal("off mapping missing")
	}
}

func TestControlsContextAndFast(t *testing.T) {
	for _, defaultFast := range []string{"true", "false"} {
		for _, fast := range []string{"", "true", "false"} {
			id := encodeID("model", "500k", fast)
			entry := &ModelEntry{ID: id}
			cache := map[string]int{"model@500k": 256000, "model": 128000, "default": 64000}
			want := 500000
			if fast == "" || fast == defaultFast {
				want = 256000
			}
			if got := contextWindowFor(cache, entry, "500k", "model", "model", fast, defaultFast); got != want {
				t.Fatalf("default=%s fast=%s: %d != %d", defaultFast, fast, got, want)
			}
			cache[id] = 400000
			if got := contextWindowFor(cache, entry, "500k", "model", "model", fast, defaultFast); got != 400000 {
				t.Fatal("exact cache lost", got)
			}
		}
	}
	for _, tc := range []struct {
		context string
		cache   map[string]int
		want    int
	}{
		{"1m", map[string]int{"model": 128000, "default": 64000}, 1000000},
		{"272k", map[string]int{"model": 1000000}, 272000},
		{"unknown", map[string]int{"model": 250000}, 250000},
		{"", map[string]int{"default": 200000}, 200000},
		{"", nil, fallbackContextWindow},
	} {
		entry := &ModelEntry{ID: encodeID("model", tc.context, "")}
		if got := contextWindowFor(tc.cache, entry, tc.context, "model", "model", "", "true"); got != tc.want {
			t.Fatalf("context %s: %d != %d", tc.context, got, tc.want)
		}
	}
	entry := &ModelEntry{Params: []Param{{ID: "fast", Value: "true"}}, SupportsFast: true}
	plain := resolvedParams(entry, ModelOverride{})
	slow := resolvedParams(entry, ModelOverride{HasFast: true, Fast: false})
	if paramValue(plain, "fast") != "true" || paramValue(slow, "fast") != "false" {
		t.Fatal(plain, slow)
	}
	if canonicalParamSignature(plain) == canonicalParamSignature(slow) {
		t.Fatal("fast signature unchanged")
	}
	entry.SupportsFast = false
	if !reflect.DeepEqual(resolvedParams(entry, ModelOverride{HasFast: true}), entry.Params) {
		t.Fatal("unsupported fast changed defaults")
	}
}

func TestControlsBareAliases(t *testing.T) {
	items, err := FallbackCatalog()
	if err != nil {
		t.Fatal(err)
	}
	catalog := BuildCatalog(items, nil)
	for _, suffix := range []string{"", ":fast", ":slow"} {
		if catalog.Lookup("grok-4.7"+suffix) != catalog.Lookup("grok-4.7@500k"+suffix) {
			t.Fatal("alias did not select native default", suffix)
		}
	}
	if paramValue(catalog.Lookup("grok-4.7@256k").Params, "context") != "256k" {
		t.Fatal("explicit context lost")
	}
	for _, entry := range catalog.Entries {
		if entry.ID == "grok-4.7" {
			t.Fatal("alias added to menu")
		}
	}
	if len(catalog.PigModels()) != catalog.Size() {
		t.Fatal("menu size changed")
	}
}

func TestControlsCanonicalSignature(t *testing.T) {
	first := []Param{{ID: "z", Value: "2"}, {ID: "a", Value: "1"}}
	before := append([]Param(nil), first...)
	second := []Param{first[1], first[0]}
	if canonicalParamSignature(first) != canonicalParamSignature(second) {
		t.Fatal("order affects signature")
	}
	if !reflect.DeepEqual(first, before) {
		t.Fatal("signature mutated params")
	}
	if canonicalParamSignature([]Param{{ID: "a", Value: "b,c=d"}}) == canonicalParamSignature([]Param{{ID: "a", Value: "b"}, {ID: "c", Value: "d"}}) {
		t.Fatal("delimiter collision")
	}
	if canonicalParamSignature(nil) != canonicalParamSignature([]Param{}) {
		t.Fatal("empty signatures differ")
	}
}
