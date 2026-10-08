package store

import (
	"fmt"
	"strconv"
	"strings"
)

// A model target entry is either one "<prefix>/<model>" or a group of
// same-priority targets sharing traffic by weight:
//
//	kimi/k3*3 | kimi-2/k3
//
// Members are separated by "|"; "*N" sets a member's weight (default 1).

// TargetMember is one target of an entry with its weight.
type TargetMember struct {
	Target string
	Weight int
}

const maxTargetWeight = 1000

// ParseTargetEntry splits an entry into its members. Malformed weights are
// treated as part of the model name (and rejected by validation).
func ParseTargetEntry(entry string) []TargetMember {
	var out []TargetMember
	for _, part := range strings.Split(entry, "|") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		m := TargetMember{Target: part, Weight: 1}
		if i := strings.LastIndex(part, "*"); i > 0 {
			if w, err := strconv.Atoi(strings.TrimSpace(part[i+1:])); err == nil {
				m.Target, m.Weight = strings.TrimSpace(part[:i]), w
			}
		}
		out = append(out, m)
	}
	return out
}

// FormatTargetEntry is the canonical form of a group (weight 1 omitted).
func FormatTargetEntry(members []TargetMember) string {
	parts := make([]string, len(members))
	for i, m := range members {
		parts[i] = m.Target
		if m.Weight != 1 {
			parts[i] += "*" + strconv.Itoa(m.Weight)
		}
	}
	return strings.Join(parts, " | ")
}

// normalizeTargetEntry validates an entry and returns its canonical form.
func normalizeTargetEntry(entry string) (string, error) {
	members := ParseTargetEntry(entry)
	if len(members) == 0 {
		return "", fmt.Errorf("empty target %q", entry)
	}
	seen := map[string]bool{}
	var kept []TargetMember
	for _, m := range members {
		i := strings.Index(m.Target, "/")
		if i <= 0 || i == len(m.Target)-1 {
			return "", fmt.Errorf("target %q must look like <prefix>/<model>", m.Target)
		}
		if m.Weight < 1 || m.Weight > maxTargetWeight {
			return "", fmt.Errorf("weight of %q must be 1-%d", m.Target, maxTargetWeight)
		}
		if seen[m.Target] {
			continue
		}
		seen[m.Target] = true
		kept = append(kept, m)
	}
	return FormatTargetEntry(kept), nil
}

// renameEntryPrefix rewrites members that use oldPrefix.
func renameEntryPrefix(entry, oldPrefix, newPrefix string) (string, bool) {
	members := ParseTargetEntry(entry)
	changed := false
	for i, m := range members {
		if strings.HasPrefix(m.Target, oldPrefix+"/") {
			members[i].Target = newPrefix + m.Target[len(oldPrefix):]
			changed = true
		}
	}
	if !changed {
		return entry, false
	}
	return FormatTargetEntry(members), true
}
