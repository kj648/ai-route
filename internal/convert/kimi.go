package convert

import (
	"regexp"
	"strings"
)

// kimiFixedName matches the Kimi models whose sampling parameters are fixed
// (platform.kimi.ai model parameter reference): kimi-k2.5, kimi-k2.6,
// kimi-k2.7-code[-highspeed] and kimi-k3, under the names vendors list them
// (k3 / k3-256k on Kimi Code, moonshotai/kimi-k3 on OpenRouter).
var kimiFixedName = regexp.MustCompile(`(?:^|[/.])(?:kimi-(?:k2\.[5-7]|k3)|k3)(?:$|[-.:])`)

// fixedSampling reports models that reject any sampling value other than the
// vendor's own (400 "invalid temperature: only 1 is allowed for this model").
// kimi-k2.6 even fixes a different temperature per thinking mode, so the
// fields are dropped rather than set and the upstream applies its defaults.
func fixedSampling(model string) bool {
	return kimiFixedName.MatchString(strings.ToLower(model))
}

// samplingFields are the request fields fixedSampling models reject. n is
// fixed at 1 too but kept: silently returning one choice when the client
// asked for several is worse than the upstream's error.
var samplingFields = []string{"temperature", "top_p", "presence_penalty", "frequency_penalty"}
