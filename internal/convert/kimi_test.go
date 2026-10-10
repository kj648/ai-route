package convert

import "testing"

func TestFixedSampling(t *testing.T) {
	for model, want := range map[string]bool{
		"k3":                       true,
		"k3-256k":                  true,
		"kimi-k3":                  true,
		"Kimi-K3":                  true,
		"moonshotai/kimi-k3":       true,
		"kimi-k2.5":                true,
		"kimi-k2.6":                true,
		"kimi-k2.7-code":           true,
		"kimi-k2.7-code-highspeed": true,
		"moonshotai/kimi-k2.6":     true,
		"kimi-k2":                  false,
		"kimi-k2.4":                false,
		"kimi-for-coding":          false,
		"deepseek-v4-flash":        false,
		"k30":                      false,
		"gpt-5-k3x":                false,
	} {
		if got := fixedSampling(model); got != want {
			t.Errorf("%s: got %v", model, got)
		}
	}
}

func TestFixedSamplingDropsFields(t *testing.T) {
	const body = `{"model":"deepseek-flash","temperature":0.3,"top_p":0.9,"presence_penalty":0.5,"frequency_penalty":0.5,"n":1,"max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`
	for _, upstream := range []string{ProtoOpenAI, ProtoAnthropic, ProtoResponses} {
		out, _, err := ConvertRequest(ProtoOpenAI, upstream, []byte(body), "k3", 1024, false)
		if err != nil {
			t.Fatal(err)
		}
		m := decodeMap(t, out)
		if _, ok := m["temperature"]; ok {
			t.Errorf("%s: temperature kept: %s", upstream, out)
		}
		for _, k := range []string{"top_p", "presence_penalty", "frequency_penalty"} {
			if _, ok := m[k]; ok {
				t.Errorf("%s: %s kept: %s", upstream, k, out)
			}
		}
		if upstream == ProtoOpenAI && m["n"] != 1.0 {
			t.Errorf("n dropped: %s", out)
		}
	}
	// other models keep the client's values
	out, _, err := ConvertRequest(ProtoOpenAI, ProtoOpenAI, []byte(body), "deepseek-v4-flash", 1024, false)
	if err != nil {
		t.Fatal(err)
	}
	if m := decodeMap(t, out); m["temperature"] != 0.3 || m["top_p"] != 0.9 || m["presence_penalty"] != 0.5 {
		t.Fatalf("sampling dropped: %s", out)
	}
}
