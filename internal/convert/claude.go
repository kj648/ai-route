package convert

import (
	"regexp"
	"strconv"
	"strings"
)

// claudeTraits describes request restrictions of newer Claude models, which
// matter when an OpenAI-format request is converted for them.
type claudeTraits struct {
	adaptive     bool // reasoning goes through thinking:{type:adaptive} + output_config.effort
	noBudget     bool // thinking budget_tokens is rejected (400)
	noSampling   bool // temperature / top_p are rejected (400)
	noForcedTool bool // tool_choice any / tool is rejected (400)
	isClaude     bool
	family       string
	major, minor int
}

// claudeName matches claude-<family>-<major>[-<minor>] anywhere in a model
// id, e.g. claude-opus-4-8, anthropic/claude-sonnet-5, us.anthropic.claude-fable-5-1.
// Dated snapshot suffixes (-20250929) are not mistaken for a minor version.
var claudeName = regexp.MustCompile(`claude-(opus|sonnet|haiku|fable|mythos)-(\d+)(?:[-.](\d{1,2}))?(?:\D|$)`)

func traitsFor(model string) claudeTraits {
	m := claudeName.FindStringSubmatch(strings.ToLower(model))
	if m == nil {
		return claudeTraits{}
	}
	t := claudeTraits{isClaude: true, family: m[1]}
	t.major, _ = strconv.Atoi(m[2])
	if m[3] != "" {
		t.minor, _ = strconv.Atoi(m[3])
	}
	atLeast := func(major, minor int) bool { return t.major > major || t.major == major && t.minor >= minor }
	switch t.family {
	case "fable", "mythos":
		t.adaptive, t.noBudget, t.noSampling = true, true, true
		t.noForcedTool = atLeast(5, 1)
	case "opus":
		t.adaptive = atLeast(4, 6)
		t.noBudget = atLeast(4, 7)
		t.noSampling = atLeast(4, 7)
		t.noForcedTool = atLeast(5, 5)
	case "sonnet":
		t.adaptive = atLeast(4, 6)
		t.noBudget = atLeast(5, 0)
		t.noSampling = atLeast(5, 0)
		t.noForcedTool = atLeast(5, 5)
	}
	return t
}

// effortLevel maps OpenAI reasoning_effort onto Anthropic output_config.effort.
func effortLevel(effort string) string {
	switch effort {
	case "minimal", "low":
		return "low"
	case "medium":
		return "medium"
	case "high":
		return "high"
	case "xhigh", "max":
		return effort
	}
	return ""
}
